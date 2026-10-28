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
)

// Dest écrit les fichiers restaurés sous un dossier, sans jamais en sortir : chaque
// écriture passe par os.Root (les liens qui mèneraient hors du dossier sont refusés par
// le système, même posés pendant la restauration), et tout lien symbolique ou jonction
// rencontré sur le chemin arrête le fichier concerné.
type Dest struct {
	Dir   string
	root  *os.Root
	owner Owner
}

// Owner : propriétaire donné aux fichiers et dossiers créés (Ubuntu, macOS). Sous
// Windows, ils héritent des droits du dossier qui les reçoit et Owner est ignoré.
type Owner struct {
	Set      bool
	UID, GID int
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
	return &Dest{Dir: dir, root: r, owner: owner}, nil
}

// Close libère le dossier.
func (d *Dest) Close() error { return d.root.Close() }

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
			if err := d.root.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if err := d.own(cur, 0o755); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

func (d *Dest) own(name string, mode fs.FileMode) error {
	if !d.owner.Set {
		return nil
	}
	if err := d.root.Chmod(name, mode); err != nil {
		return err
	}
	return d.root.Lchown(name, d.owner.UID, d.owner.GID)
}

// Done : le fichier rel existe déjà avec la taille attendue (reprise).
func (d *Dest) Done(rel string, size int64) bool {
	fi, err := d.root.Lstat(rel)
	return err == nil && fi.Mode().IsRegular() && fi.Size() == size
}

// Write lit la version v et l'écrit en rel : d'abord dans un fichier temporaire du même
// dossier, taille (et empreinte quand le bucket la connaît) contrôlée, date de la
// sauvegarde posée, puis renommé à son nom. Rien n'est écrasé.
func (d *Dest) Write(ctx context.Context, g Getter, rel string, v Version) (err error) {
	if p := CheckRel(rel); p != "" {
		return errors.New(p)
	}
	dir := path.Dir(rel)
	if err := d.mkdirs(dir); err != nil {
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
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := path.Join(dir, ".kura-restore-"+hex.EncodeToString(rnd[:])+".tmp")
	f, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
		if err != nil {
			_ = d.root.Remove(tmp)
		}
	}()
	obj, err := g.GetVersion(ctx, v.Key, v.VersionID)
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(f, h), ctxReader{ctx, obj.Body})
	if err != nil {
		return fmt.Errorf("lecture interrompue : %w", err)
	}
	if n != v.Size || (obj.Size >= 0 && obj.Size != v.Size) {
		return fmt.Errorf("taille reçue %d octets, %d attendus : version altérée en route", n, v.Size)
	}
	if obj.MD5 != "" && hex.EncodeToString(h.Sum(nil)) != obj.MD5 {
		return errors.New("empreinte différente de celle du bucket : version altérée en route")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	cerr := f.Close()
	f = nil
	if cerr != nil {
		return cerr
	}
	if !v.LastModified.IsZero() {
		if err := d.root.Chtimes(tmp, v.LastModified, v.LastModified); err != nil {
			return err
		}
	}
	if err := d.own(tmp, 0o644); err != nil {
		return err
	}
	// Dernier contrôle juste avant le renommage : la place est toujours libre.
	if _, err := d.root.Lstat(rel); err == nil {
		return ErrExists
	}
	return d.root.Rename(tmp, rel)
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
