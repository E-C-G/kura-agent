package restore

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// Dest écrit les fichiers restaurés sous un dossier, sans jamais en sortir : chaque
// écriture passe par os.Root (les liens qui mèneraient hors du dossier sont refusés par
// le système, même posés pendant la restauration), et tout lien symbolique ou jonction
// rencontré sur le chemin arrête le fichier concerné.
//
// Droits des fichiers restaurés : le bucket ne garde ni les droits ni les ACL d'origine.
// Un fichier créé l'est donc en privé (Ubuntu, macOS : 0600, dossiers 0700 ; Windows :
// droits du dossier qui le reçoit, ou administrateurs seuls avec Private) ; un fichier
// remplacé reprend les droits du fichier actuel (mode et propriétaire, ou ACL).
type Dest struct {
	Dir string
	// Stamp : date ajoutée au nom du fichier actuel gardé lors d'un remplacement
	// (nom.avant-restauration-<Stamp>).
	Stamp string
	// Private : fichiers lisibles par les seuls administrateurs et SYSTEM (Windows ;
	// ailleurs, Owner root et 0600 font de même). Restauration dans un dossier à part :
	// elle mélange les fichiers de tous les comptes.
	Private bool
	root    *os.Root
	owner   Owner
	staging string // Windows : dossier de travail privé des remplacements ("" : aucun)
}

// Modes des fichiers et dossiers créés (Ubuntu, macOS).
const (
	fileMode = 0o600
	dirMode  = 0o700
)

// Les droits ne sont jamais appliqués par chemin : toujours sur le descripteur du fichier
// encore ouvert (fchmod, fchown, SetSecurityInfo). Un chemin peut être remplacé par un
// lien entre deux appels ; un descripteur désigne le fichier que l'agent a créé.

// Owner : propriétaire donné aux fichiers et dossiers créés (Ubuntu, macOS). Sous
// Windows, ils héritent des droits du dossier qui les reçoit et Owner est ignoré.
type Owner struct {
	Set      bool
	UID, GID int
	// Inherit : propriétaire et groupe du dossier qui reçoit chaque fichier (restauration
	// en place, dans des dossiers de comptes différents) ; UID et GID sont ignorés.
	Inherit bool
}

// ErrLink : lien symbolique ou jonction dans la destination.
var ErrLink = errors.New("lien symbolique ou jonction dans la destination : fichier non restauré")

// ErrExists : un fichier occupe déjà la place.
var ErrExists = errors.New("un fichier existe déjà à cet emplacement")

// OpenDest ouvre la destination, qui doit exister et être un vrai dossier.
func OpenDest(dir string, owner Owner) (*Dest, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if isLink(fi) || !fi.IsDir() {
		return nil, fmt.Errorf("%s : %w", dir, ErrLink)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	// Le dossier ouvert est bien celui examiné : pas remplacé par un lien entre-temps.
	if got, err := r.Stat("."); err != nil || !os.SameFile(fi, got) {
		r.Close()
		return nil, fmt.Errorf("%s : %w", dir, ErrLink)
	}
	return &Dest{Dir: dir, root: r, owner: owner}, nil
}

// Close libère le dossier (et retire le dossier de travail des remplacements).
func (d *Dest) Close() error {
	if d.staging != "" {
		_ = d.root.Remove(d.staging)
	}
	return d.root.Close()
}

func isLink(fi fs.FileInfo) bool {
	return fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0
}

// mkdirs crée les dossiers de rel, un par un, en refusant tout lien.
func (d *Dest) mkdirs(rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	cur := ""
	for _, seg := range strings.Split(rel, "/") {
		cur = path.Join(cur, seg)
		fi, err := d.root.Lstat(cur)
		switch {
		case err == nil:
			if isLink(fi) {
				return fmt.Errorf("%s : %w", cur, ErrLink)
			}
			if !fi.IsDir() {
				return fmt.Errorf("%s : un fichier occupe la place d'un dossier", cur)
			}
		case errors.Is(err, fs.ErrNotExist):
			if err := d.root.Mkdir(cur, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if err := d.own(cur, dirMode); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

// ownerFor : propriétaire à donner à un fichier ou dossier créé sous dir (Owner).
func (d *Dest) ownerFor(dir string) (uid, gid int, ok bool, err error) {
	if !d.owner.Set {
		return 0, 0, false, nil
	}
	if !d.owner.Inherit {
		return d.owner.UID, d.owner.GID, true, nil
	}
	fi, err := d.root.Stat(dir)
	if err != nil {
		return 0, 0, false, err
	}
	uid, gid, ok = fileOwner(fi)
	return uid, gid, ok, nil
}

// own : mode et propriétaire d'un dossier créé, par son descripteur.
func (d *Dest) own(name string, mode fs.FileMode) error {
	uid, gid, ok, err := d.ownerFor(path.Dir(name))
	if err != nil || !ok {
		return err
	}
	f, err := d.root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s : %w", name, ErrLink)
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	return f.Chown(uid, gid)
}

// Done : le fichier rel existe déjà avec la taille attendue (reprise).
func (d *Dest) Done(rel string, size int64) bool {
	fi, err := d.root.Lstat(rel)
	return err == nil && fi.Mode().IsRegular() && fi.Size() == size
}

// Write lit la version v et l'écrit en rel : d'abord dans un fichier temporaire du même
// dossier, taille (et empreinte quand le bucket la connaît) contrôlée, date de la
// sauvegarde posée, puis renommé à son nom. Rien n'est écrasé.
func (d *Dest) Write(ctx context.Context, g Getter, rel string, v Version) error {
	if err := d.prepare(rel); err != nil {
		return err
	}
	if fi, err := d.root.Lstat(rel); err == nil {
		if isLink(fi) {
			return ErrLink
		}
		return ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := d.fetch(ctx, g, rel, v)
	if err != nil {
		return err
	}
	return d.place(tmp, rel)
}

// place donne au temporaire son nom sans jamais remplacer un fichier apparu entre-temps
// (une application qui enregistre au même moment) : un lien physique se crée de façon
// exclusive, puis le temporaire est retiré. Sur un système de fichiers sans liens
// physiques (FAT, exFAT), dernier contrôle puis renommage.
func (d *Dest) place(tmp, rel string) error {
	defer func() { // sans effet une fois renommé ; un antivirus peut le tenir un instant
		for i := 0; i < 5; i++ {
			if err := d.root.Remove(tmp); err == nil || errors.Is(err, fs.ErrNotExist) {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	err := d.root.Link(tmp, rel)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return ErrExists
	case !linkUnsupported(err):
		return err
	}
	if _, err := d.root.Lstat(rel); err == nil {
		return ErrExists
	}
	return d.root.Rename(tmp, rel)
}

// Replace remplace le fichier rel par la version v en gardant le fichier actuel à côté,
// sous le nom renvoyé (rel.avant-restauration-<stamp>). Un fichier ouvert par une
// application (Outlook) n'est pas remplacé : le renommage échoue et rien ne change.
func (d *Dest) Replace(ctx context.Context, g Getter, rel string, v Version, stamp string) (string, error) {
	if err := d.prepare(rel); err != nil {
		return "", err
	}
	fi, err := d.root.Lstat(rel)
	if err != nil {
		return "", fmt.Errorf("%s : %w", errBusyFile, err)
	}
	if isLink(fi) {
		return "", ErrLink
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("un dossier occupe la place du fichier")
	}
	// Droits du fichier actuel, lus par un descripteur ouvert dans la destination.
	cur, err := d.root.Open(rel)
	if err != nil {
		return "", fmt.Errorf("%s : %w", errBusyFile, err)
	}
	ref, err := readRights(cur)
	cur.Close()
	if err != nil {
		return "", fmt.Errorf("droits du fichier actuel illisibles, rien n'a changé : %w", err)
	}
	// Sous Windows, écrit dans un dossier de travail privé : le fichier n'hérite jamais,
	// même un instant, des droits plus larges du dossier qui le recevra.
	dir, err := d.stagingDir()
	if err != nil {
		return "", err
	}
	tmp, err := d.fetchIn(ctx, g, dir, rel, v, &ref)
	if err != nil {
		return "", err
	}
	backup := ""
	for i := 1; ; i++ {
		backup = rel + ".avant-restauration-" + stamp
		if i > 1 {
			backup += fmt.Sprintf("-%d", i)
		}
		if _, err := d.root.Lstat(backup); errors.Is(err, fs.ErrNotExist) {
			break
		}
		if i > 99 {
			_ = d.root.Remove(tmp)
			return "", errors.New("trop de copies avant-restauration à côté du fichier")
		}
	}
	if err := d.root.Rename(rel, backup); err != nil {
		_ = d.root.Remove(tmp)
		return "", fmt.Errorf("%s : %w", errBusyFile, err)
	}
	if err := d.root.Rename(tmp, rel); err != nil {
		_ = d.root.Rename(backup, rel)
		_ = d.root.Remove(tmp)
		return "", err
	}
	// Windows, ACL héritée : recalculée maintenant que le fichier est à sa place.
	if err := d.settle(rel, ref); err != nil {
		return backup, fmt.Errorf("fichier remis, droits hérités à vérifier : %w", err)
	}
	return backup, nil
}

// prepare contrôle le nom et crée ses dossiers, sans suivre de lien.
func (d *Dest) prepare(rel string) error {
	if p := CheckRel(rel); p != "" {
		return errors.New(p)
	}
	return d.mkdirs(path.Dir(rel))
}

// fetch lit la version dans un fichier temporaire à côté de rel, contrôlé et daté ;
// renvoie son nom. Le temporaire est effacé en cas d'erreur.
func (d *Dest) fetch(ctx context.Context, g Getter, rel string, v Version) (string, error) {
	return d.fetchIn(ctx, g, path.Dir(rel), rel, v, nil)
}

// fetchIn : fetch avec le temporaire dans dir ; ref, s'il est donné, sont les droits à
// reproduire (fichier remplacé), appliqués au descripteur avant sa fermeture.
func (d *Dest) fetchIn(ctx context.Context, g Getter, dir, rel string, v Version, ref *rights) (tmp string, err error) {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	name := path.Join(dir, ".kura-restore-"+hex.EncodeToString(rnd[:])+".tmp")
	f, err := d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return "", err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
		if err != nil {
			_ = d.root.Remove(name)
		}
	}()
	obj, err := g.GetVersion(ctx, v.Key, v.VersionID)
	if err != nil {
		return "", err
	}
	defer obj.Body.Close()
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(f, h), ctxReader{ctx, obj.Body})
	if err != nil {
		return "", fmt.Errorf("lecture interrompue : %w", err)
	}
	if n != v.Size || (obj.Size >= 0 && obj.Size != v.Size) {
		return "", fmt.Errorf("taille reçue %d octets, %d attendus : version altérée en route", n, v.Size)
	}
	if obj.MD5 != "" && hex.EncodeToString(h.Sum(nil)) != obj.MD5 {
		return "", errors.New("empreinte différente de celle du bucket : version altérée en route")
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	// Droits sur le descripteur encore ouvert, jamais sur le chemin.
	switch {
	case ref != nil:
		err = applyRights(f, *ref)
	case d.Private:
		err = applyRights(f, privateRights())
	default:
		var uid, gid int
		var ok bool
		if uid, gid, ok, err = d.ownerFor(path.Dir(rel)); err == nil && ok {
			if err = f.Chmod(fileMode); err == nil {
				err = f.Chown(uid, gid)
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("droits du fichier restauré : %w", err)
	}
	if !v.LastModified.IsZero() {
		if err = setTimes(f, v.LastModified); err != nil {
			return "", err
		}
	}
	err = f.Close()
	f = nil
	if err != nil {
		return "", err
	}
	return name, nil
}

// ctxReader arrête la lecture quand le contexte est annulé (Ctrl+C).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
