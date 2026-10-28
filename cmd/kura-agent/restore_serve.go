package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/E-C-G/kura-agent/internal/ipc"
	"github.com/E-C-G/kura-agent/internal/restore"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Restauration depuis l'icône : kura-agent restore --serve, lancé par la tâche
// \Kura\Restore (SYSTEM) quand un compte de tray.users ouvre la fenêtre de
// restauration. La tâche joint la fenêtre de chaque session par son canal, ne montre
// au compte que les dossiers sauvegardés où il peut écrire, et n'écrit qu'en son nom.

// trayUser : le compte de l'icône servie, vu par la tâche.
type trayUser interface {
	Account() string // DOMAINE\nom
	Home() string    // dossier de profil
	// CanWrite : le compte peut lister dir et y créer des fichiers (contrôle fait par
	// le système avec le jeton du compte).
	CanWrite(dir string) error
	// CanWriteAll : CanWrite pour chaque dossier, en un seul passage au nom du compte.
	CanWriteAll(dirs []string) []error
	// CanReadAll : le compte peut lire chaque fichier (un fichier ouvert sans partage
	// par une application compte comme lisible : Windows contrôle les droits avant).
	CanReadAll(files []string) []error
	// Do exécute f sur un thread verrouillé, au nom du compte.
	Do(f func() error) error
	// Impersonate prend l'identité du compte sur le thread courant (verrouillé par
	// l'appelant) ; la fonction rendue la quitte.
	Impersonate() (func(), error)
	Close()
}

// serveDeps : ce que le serveur demande au système, remplaçable dans les tests.
type serveDeps struct {
	restoreDeps
	sessions func() ([]int, error)
	dial     func(session int) (io.ReadWriteCloser, trayUser, error)
	poll     time.Duration // recherche des fenêtres ouvertes
	idle     time.Duration // connexion sans demande : fermée
	linger   time.Duration // plus aucune fenêtre : fin du processus
	logw     io.Writer
}

const (
	trayRetentionDays = 90
	trayMaxFiles      = 50000 // au-delà, choisir un sous-dossier
	trayMaxVersions   = 500
)

// serveRestore sert les fenêtres de restauration jusqu'à ce qu'aucune ne soit plus
// ouverte depuis linger.
func serveRestore(ctx context.Context, sd serveDeps, cfgPath string) int {
	logf := func(format string, a ...any) {
		fmt.Fprintf(sd.logw, "%s "+format+"\n", append([]any{time.Now().Format("2006-01-02 15:04:05")}, a...)...)
	}
	if err := sd.checkAdmin(); err != nil {
		logf("refusé : %v", err)
		return exitRestoreRefused
	}
	cfg, err := sd.loadConfig(cfgPath)
	if err != nil {
		logf("configuration : %v", err)
		return exitRestoreConfig
	}
	b, err := sd.bucket(cfg)
	if err != nil {
		logf("clé du poste : %v", err)
		return exitRestoreConfig
	}
	logf("restauration depuis l'icône : en attente des fenêtres")
	var mu sync.Mutex
	active := map[int]bool{}
	last := time.Now()
	var wg sync.WaitGroup
	defer wg.Wait()
	tick := time.NewTicker(sd.poll)
	defer tick.Stop()
	for {
		// Recherche des fenêtres d'abord : une fenêtre ouverte juste avant la fin est
		// jointe (la tâche, déjà lancée, ignore un nouveau lancement).
		if list, err := sd.sessions(); err == nil {
			for _, s := range list {
				mu.Lock()
				busy := active[s]
				mu.Unlock()
				if busy {
					continue
				}
				conn, u, err := sd.dial(s)
				if err != nil {
					continue // pas de fenêtre ouverte dans cette session
				}
				mu.Lock()
				active[s] = true
				mu.Unlock()
				logf("session %d : fenêtre de %s jointe", s, u.Account())
				wg.Add(1)
				go func(s int) {
					defer wg.Done()
					defer u.Close()
					(&traySession{sd: sd, cfg: cfg, b: b, u: u, logf: logf}).serve(ctx, conn)
					logf("session %d : fenêtre fermée", s)
					mu.Lock()
					delete(active, s)
					last = time.Now()
					mu.Unlock()
				}(s)
			}
		}
		mu.Lock()
		n := len(active)
		if n > 0 {
			last = time.Now()
		}
		idle := time.Since(last)
		mu.Unlock()
		if n == 0 && idle > sd.linger {
			logf("plus aucune fenêtre : fin")
			return exitRestoreOK
		}
		select {
		case <-ctx.Done():
			return exitRestoreOK
		case <-tick.C:
		}
	}
}

// traySession : une fenêtre de restauration servie.
type traySession struct {
	sd   serveDeps
	cfg  *config.Config
	b    restoreBucket
	u    trayUser
	logf func(string, ...any)

	listing *trayListing
	picks   map[string]bool
}

// trayListing : dernière liste envoyée à la fenêtre.
type trayListing struct {
	path     string // dossier choisi, tel que le système l'écrit
	target   restore.Target
	at       time.Time
	versions []restore.Version // de la cible seulement
	files    []restore.File    // visibles par le compte
	names    map[string]bool   // Rel des fichiers visibles
}

func (ts *traySession) serve(ctx context.Context, conn io.ReadWriteCloser) {
	msgs := make(chan ipc.Msg, 64)
	var peer *ipc.Peer
	ready := make(chan struct{})
	peer = ipc.NewPeer(conn, func(m ipc.Msg) {
		<-ready
		// Contre-pression : la lecture attend que la demande précédente soit traitée,
		// rien n'est perdu.
		select {
		case msgs <- m:
		case <-peer.Done():
		}
	})
	close(ready)
	defer peer.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-peer.Done()
		cancel()
	}()

	hello := ipc.Msg{Type: ipc.TypeRestoreHello, Version: ts.sd.version, Restore: &ipc.RestoreData{Account: ts.u.Account(), Days: trayRetentionDays}}
	if !config.Listed(ts.cfg.Tray.Users, ts.u.Account()) {
		hello.Message = "Le compte " + ts.u.Account() + " ne figure pas dans tray.users : restauration réservée aux comptes de l'icône."
		_ = peer.Send(hello)
		ts.logf("%s refusé : absent de tray.users", ts.u.Account())
		return
	}
	hello.Restore.Roots = ts.roots()
	if err := peer.Send(hello); err != nil {
		return
	}
	idle := time.NewTimer(ts.sd.idle)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			ts.logf("%s : %s sans demande, fenêtre libérée", ts.u.Account(), ts.sd.idle)
			return
		case m := <-msgs:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			ts.handle(ctx, peer, m)
			idle.Reset(ts.sd.idle)
		}
	}
}

// roots : dossiers sauvegardés que le compte peut restaurer (raccourcis de la fenêtre).
func (ts *traySession) roots() []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		k := strings.ToLower(p)
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	home := ts.u.Home()
	for _, j := range ts.cfg.Jobs {
		if j.Unavailable != "" || j.Source == "" {
			continue
		}
		src := filepath.Clean(j.Source)
		if ts.u.CanWrite(src) == nil {
			add(src)
			continue
		}
		// Source commune (C:\Users) : le profil du compte, s'il en relève.
		if home != "" {
			if t, err := restore.Resolve(ts.cfg, home, ts.sd.windows); err == nil && t.Job.ID == j.ID && ts.u.CanWrite(home) == nil {
				add(home)
			}
		}
	}
	return out
}

func (ts *traySession) handle(ctx context.Context, peer *ipc.Peer, m ipc.Msg) {
	reply := func(r ipc.Msg) { _ = peer.Reply(m, r) }
	fail := func(typ, format string, a ...any) {
		reply(ipc.Msg{Type: typ, Message: fmt.Sprintf(format, a...), Restore: &ipc.RestoreData{}})
	}
	rd := m.Restore
	if rd == nil {
		rd = &ipc.RestoreData{}
	}
	switch m.Type {
	case ipc.TypeRestoreList:
		ts.list(ctx, peer, m, rd)
	case ipc.TypeRestoreHistory:
		if ts.listing == nil {
			fail(ipc.TypeRestoreVersions, "Afficher d'abord un dossier.")
			return
		}
		if !ts.listing.names[rd.Rel] {
			fail(ipc.TypeRestoreVersions, "Fichier inconnu.")
			return
		}
		key := strings.TrimPrefix(ts.listing.target.Key+"/"+rd.Rel, "/")
		var vs []ipc.RestoreVersion
		for _, v := range restore.History(ts.listing.versions, key, ts.sd.windows) {
			if len(vs) == trayMaxVersions {
				break
			}
			vs = append(vs, ipc.RestoreVersion{Date: v.LastModified, Size: v.Size, Deleted: v.Deleted, Current: v.Latest})
		}
		reply(ipc.Msg{Type: ipc.TypeRestoreVersions, Restore: &ipc.RestoreData{Rel: rd.Rel, Versions: vs}})
	case ipc.TypeRestorePick:
		if ts.listing == nil {
			return
		}
		if ts.picks == nil {
			ts.picks = map[string]bool{}
		}
		for _, r := range rd.Rels {
			if ts.listing.names[r] { // seulement des fichiers de la liste envoyée
				ts.picks[r] = true
			}
		}
	case ipc.TypeRestoreRun:
		ts.run(ctx, peer, m, rd)
	}
}

// list : état du dossier choisi à la date demandée, réduit à ce que le compte peut voir.
func (ts *traySession) list(ctx context.Context, peer *ipc.Peer, m ipc.Msg, rd *ipc.RestoreData) {
	fail := func(format string, a ...any) {
		_ = peer.Reply(m, ipc.Msg{Type: ipc.TypeRestoreListed, Message: fmt.Sprintf(format, a...), Restore: &ipc.RestoreData{}})
	}
	ts.listing, ts.picks = nil, nil
	dir, err := ts.allowed(m.Path)
	if err != nil {
		fail("%v", err)
		return
	}
	now := ts.sd.now()
	var at time.Time
	if rd.At != nil && rd.At.Before(now) {
		if now.Sub(*rd.At) > time.Duration(trayRetentionDays)*24*time.Hour {
			fail("%d jours au plus : les versions plus anciennes sont effacées par le cycle de vie du bucket.", trayRetentionDays)
			return
		}
		at = *rd.At
	}
	t, _ := restore.Resolve(ts.cfg, dir, ts.sd.windows)
	lctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// Préfixe exact seulement, jamais de relecture du job entier depuis l'icône : le
	// sélecteur de dossiers donne la casse du disque, celle des clés.
	vs, err := ts.b.ListVersions(lctx, t.Prefix())
	if err != nil {
		ts.logf("%s : liste de %s : %v", ts.u.Account(), dir, err)
		fail("Bucket injoignable : %v", err)
		return
	}
	// Seules les versions de la cible restent en mémoire (la relecture sans casse lit
	// le job entier).
	var mine []restore.Version
	for _, v := range vs {
		if t.Covers(v.Key, ts.sd.windows) {
			mine = append(mine, v)
		}
	}
	files := restore.StateAt(mine, t, at, ts.sd.windows)
	if !t.Root && len(files) == 1 && len(files[0].Key) == len(t.Key) {
		fail("%s est un fichier : choisir le dossier qui le contient.", dir)
		return
	}
	files, hidden := ts.visible(dir, files)
	var notes []string
	if hidden {
		notes = append(notes, "Certains fichiers ou dossiers ne vous sont pas accessibles : ils ne sont pas listés.")
	}
	if len(files) > trayMaxFiles {
		files = files[:trayMaxFiles]
		notes = append(notes, fmt.Sprintf("Plus de %d fichiers : seuls les %d premiers sont listés ; choisir un sous-dossier.", trayMaxFiles, trayMaxFiles))
	}
	msg := strings.Join(notes, " ")
	l := &trayListing{path: dir, target: t, at: at, versions: mine, files: files, names: map[string]bool{}}
	var out []ipc.RestoreFile
	var total int64
	for _, f := range files {
		l.names[f.Rel] = true
		out = append(out, ipc.RestoreFile{Rel: f.Rel, Size: f.Version.Size, BackedUp: f.Version.LastModified,
			DeletedSince: f.DeletedSince, Newer: f.NewerSince, Problem: f.Problem})
		total += f.Version.Size
	}
	ts.listing = l
	req := rd.Req // numéro choisi par la fenêtre : elle écarte les lots d'une liste abandonnée
	if req == 0 {
		req = m.ID
	}
	for _, chunk := range ipc.ChunkFiles(out) {
		if err := peer.Send(ipc.Msg{Type: ipc.TypeRestoreFiles, Restore: &ipc.RestoreData{Req: req, Files: chunk}}); err != nil {
			return
		}
	}
	_ = peer.Reply(m, ipc.Msg{Type: ipc.TypeRestoreListed, Path: dir, Message: msg,
		Restore: &ipc.RestoreData{Req: req, Total: len(files), Bytes: total}})
}

// within : p est home ou en dessous.
func within(home, p string, foldCase bool) bool {
	if home == "" {
		return false
	}
	home = strings.TrimRight(home, `/\`)
	if foldCase {
		home, p = strings.ToLower(home), strings.ToLower(p)
	}
	return p == home || strings.HasPrefix(p, home+string(filepath.Separator))
}

// reach : dossier où le compte doit pouvoir écrire pour restaurer dans dir. Pour un
// dossier disparu, son plus proche parent existant, mais seulement dans le profil du
// compte : ailleurs (dossier commun), un dossier supprimé a pu être celui d'un autre
// compte ; il se restaure alors en ligne de commande, par un administrateur.
func (ts *traySession) reach(dir string) (string, bool) {
	ne := nearestExisting(dir)
	if ne != dir && !within(ts.u.Home(), ne, ts.sd.windows) {
		return ne, false
	}
	return ne, true
}

// visible garde les fichiers que le compte peut restaurer, vérifiés par Windows avec son
// jeton :
//   - leur dossier (ou, disparu dans son profil, le plus proche parent existant) lui est
//     ouvert en liste et en écriture : le sous-dossier privé d'un dossier commun
//     n'apparaît pas ;
//   - un fichier encore présent lui est lisible ;
//   - hors de son profil, seuls les fichiers encore présents (et lisibles) : un fichier
//     supprimé d'un dossier commun a pu appartenir à un autre compte, et rien sur le
//     disque ne le prouve (un dossier se recrée, sa date se modifie) ; il se restaure
//     par l'administrateur, en ligne de commande.
func (ts *traySession) visible(base string, files []restore.File) ([]restore.File, bool) {
	home := ts.u.Home()
	parentOf := func(f restore.File) string {
		p := filepath.Join(base, filepath.FromSlash(path.Dir(f.Rel)))
		if !within(base, p, ts.sd.windows) {
			return base // nom refusé (..) : rattaché au dossier choisi
		}
		return p
	}
	reachOf := map[string]string{} // dossier du fichier → dossier à vérifier ("" : refusé)
	var dirs []string
	seen := map[string]bool{}
	for _, f := range files {
		p := parentOf(f)
		if _, ok := reachOf[p]; ok {
			continue
		}
		ne, ok := ts.reach(p)
		if !ok {
			reachOf[p] = ""
			continue
		}
		reachOf[p] = ne
		if !seen[ne] {
			seen[ne] = true
			dirs = append(dirs, ne)
		}
	}
	dirOK := map[string]bool{}
	for i, err := range ts.u.CanWriteAll(dirs) {
		dirOK[dirs[i]] = err == nil
	}
	// Fichiers encore présents : lisibles par le compte ?
	present := map[int]string{}
	var paths []string
	for i, f := range files {
		if f.Problem != "" {
			continue
		}
		lp := filepath.Join(base, filepath.FromSlash(f.Rel))
		if fi, err := os.Lstat(lp); err == nil && fi.Mode().IsRegular() {
			present[i] = lp
			paths = append(paths, lp)
		}
	}
	readOK := map[string]bool{}
	for i, err := range ts.u.CanReadAll(paths) {
		readOK[paths[i]] = err == nil
	}
	var out []restore.File
	hidden := false
	for i, f := range files {
		p := parentOf(f)
		ok := reachOf[p] != "" && dirOK[reachOf[p]]
		if lp, isPresent := present[i]; ok && isPresent {
			ok = readOK[lp]
		} else if ok && !within(home, p, ts.sd.windows) {
			ok = false // supprimé, hors du profil
		}
		if ok {
			out = append(out, f)
		} else {
			hidden = true
		}
	}
	return out, hidden
}

// allowed vérifie un dossier demandé par la fenêtre : chemin absolu ordinaire, dans la
// source d'un job, et le compte peut y écrire (ou, disparu dans son profil, dans son plus
// proche parent existant).
func (ts *traySession) allowed(p string) (string, error) {
	if p == "" {
		return "", errors.New("Choisir un dossier.")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("Chemin invalide.")
		}
	}
	dir := filepath.Clean(p)
	if ts.sd.windows {
		if len(dir) < 3 || dir[1] != ':' || (dir[2] != '\\' && dir[2] != '/') {
			return "", errors.New("Chemin incomplet : choisir un dossier d'un disque de ce poste.")
		}
	} else if !filepath.IsAbs(dir) {
		return "", errors.New("Chemin incomplet : choisir un dossier.")
	}
	if _, err := restore.Resolve(ts.cfg, dir, ts.sd.windows); err != nil {
		return "", fmt.Errorf("%s n'est pas sauvegardé par Kura.", dir)
	}
	ne, ok := ts.reach(dir)
	if !ok {
		return "", fmt.Errorf("%s n'existe plus. Hors de votre profil, un dossier supprimé se restaure par l'administrateur du poste.", dir)
	}
	if err := ts.u.CanWrite(ne); err != nil {
		ts.logf("%s : accès refusé à %s (%v)", ts.u.Account(), dir, err)
		return "", fmt.Errorf("Vous ne pouvez pas restaurer %s : vous n'avez pas le droit d'y écrire.", dir)
	}
	return dir, nil
}

// run : restauration des fichiers choisis, en nouveau dossier ou à leur place.
func (ts *traySession) run(ctx context.Context, peer *ipc.Peer, m ipc.Msg, rd *ipc.RestoreData) {
	l := ts.listing
	picks := ts.picks
	ts.picks = nil
	reply := func(r ipc.RestoreData, msg string, ok bool) {
		_ = peer.Reply(m, ipc.Msg{Type: ipc.TypeRestoreDone, OK: ok, Message: msg, Restore: &r})
	}
	done := func(r ipc.RestoreData, msg string) { reply(r, msg, msg == "" && r.Failed == 0) }
	if l == nil {
		done(ipc.RestoreData{}, "Afficher d'abord un dossier.")
		return
	}
	var chosen []restore.File
	for _, f := range l.files {
		if picks[f.Rel] {
			chosen = append(chosen, f)
		}
	}
	if len(chosen) == 0 {
		done(ipc.RestoreData{}, "Aucun fichier coché.")
		return
	}
	// Dossier et sous-dossiers revérifiés : les droits ont pu changer depuis la liste.
	if _, err := ts.allowed(l.path); err != nil {
		done(ipc.RestoreData{}, err.Error())
		return
	}
	chosen, _ = ts.visible(l.path, chosen)
	if len(chosen) == 0 {
		done(ipc.RestoreData{}, "Ces fichiers ne vous sont plus accessibles.")
		return
	}
	d := ts.sd.restoreDeps
	disp := func(p string) string { return dispPath(p, d.windows) }
	var root, where, mode string
	var items []restore.Item
	var kept, preFailed []keptItem
	switch rd.Mode {
	case ipc.RestoreCopy:
		// À côté du dossier choisi (ou de son plus proche parent existant).
		near, _ := ts.reach(l.path)
		parent, base := filepath.Dir(near), filepath.Base(l.path)
		switch {
		case near != l.path:
			parent = near // dossier choisi disparu : le nouveau dossier va dans son parent existant
		case parent == near || restore.CheckRel(base) != "":
			parent, base = near, "" // racine d'un lecteur : le nouveau dossier va dedans
		}
		if err := ts.u.CanWrite(parent); err != nil {
			done(ipc.RestoreData{}, fmt.Sprintf("Vous ne pouvez pas créer de dossier dans %s : choisir « À leur place ».", parent))
			return
		}
		stamp := ts.sd.now().Format("2006-01-02 15h04")
		name := "Restauration Kura " + stamp
		_ = ts.u.Do(func() error { // nom libre, vu par le compte
			for i := 2; i < 100; i++ {
				if _, err := os.Lstat(filepath.Join(parent, name)); err != nil {
					return nil // libre (ou invisible pour le compte : l'écriture le dira)
				}
				name = fmt.Sprintf("Restauration Kura %s (%d)", stamp, i)
			}
			return nil
		})
		root, where, mode = parent, filepath.Join(parent, name), "to"
		for _, f := range chosen {
			out := name + "/" + f.Rel
			if base != "" {
				out = name + "/" + base + "/" + f.Rel
			}
			items = append(items, restore.Item{File: f, Out: out})
		}
	case ipc.RestoreMissing:
		// Racine : le dossier choisi, ou s'il a disparu (dans le profil du compte) son plus
		// proche parent existant ; les dossiers manquants sont recréés au nom du compte.
		near, _ := ts.reach(l.path)
		lead, err := filepath.Rel(near, l.path)
		if err != nil || strings.HasPrefix(lead, "..") {
			done(ipc.RestoreData{}, "Dossier inaccessible.")
			return
		}
		lead = filepath.ToSlash(lead)
		root, where, mode = near, l.path, "in_place"
		for _, f := range chosen {
			out := f.Rel
			if lead != "." {
				out = lead + "/" + f.Rel
			}
			items = append(items, restore.Item{File: f, Out: out})
		}
	default:
		done(ipc.RestoreData{}, "Mode inconnu.")
		return
	}
	restore.MarkConflicts(items, d.foldCase)

	// Pas de verrou de la sauvegarde ici, contrairement à la ligne de commande : rien
	// n'est jamais remplacé depuis l'icône, et un compte standard ne doit pas pouvoir
	// bloquer la sauvegarde planifiée le temps d'une longue restauration.

	var dest *restore.Dest
	if err := ts.u.Do(func() error {
		var err error
		dest, err = restore.OpenDest(root, restore.Owner{})
		return err
	}); err != nil {
		done(ipc.RestoreData{}, fmt.Sprintf("Dossier %s inaccessible : %v", root, err))
		return
	}
	defer dest.Close()

	var toWrite []restore.Item
	var need int64
	show := func(out string) string { return disp(underSource(root, out)) }
	// À leur place : ce qui existe déjà est laissé, examiné au nom du compte (un seul
	// thread pour toute la liste).
	acts := make([]restore.Action, len(items))
	errs := make([]error, len(items))
	if mode == "in_place" {
		_ = ts.u.Do(func() error {
			for i, it := range items {
				if it.Problem == "" {
					acts[i], errs[i] = dest.Classify(it, false)
				}
			}
			return nil
		})
	}
	for i, it := range items {
		switch {
		case it.Problem != "":
			toWrite = append(toWrite, it) // signalé comme nom refusé
			continue
		case errs[i] != nil:
			preFailed = append(preFailed, keptItem{show: show(it.Out), label: "ÉCHEC", item: it, err: errs[i]})
			continue
		case acts[i] != restore.ActWrite:
			kept = append(kept, keptItem{show: show(it.Out), label: "PRÉSENT", item: it})
			continue
		}
		toWrite = append(toWrite, it)
		need += it.Version.Size
	}
	if need > 0 {
		free, err := d.freeSpace(root)
		if err == nil && float64(need)*spaceMargin > float64(free) {
			done(ipc.RestoreData{}, fmt.Sprintf("%s à restaurer, %s libres sur le disque de %s.", humanSize(need), humanSize(int64(free)), root))
			return
		}
	}
	if len(toWrite) == 0 && len(preFailed) == 0 {
		done(ipc.RestoreData{Kept: len(kept), Dir: where}, "")
		return
	}

	var last time.Time
	p := &restorePlan{mode: mode, where: disp(where), at: l.at, account: ts.u.Account(),
		reports:    []listReport{{target: l.target, files: chosen}},
		batches:    []batch{{dest: dest, items: toWrite, show: show}},
		kept:       kept,
		preFailed:  preFailed,
		workerInit: ts.u.Impersonate,
		progress: func(n, total int, bytes int64) {
			if time.Since(last) < 300*time.Millisecond && n < total {
				return
			}
			last = time.Now()
			_ = peer.Send(ipc.Msg{Type: ipc.TypeRestoreProgress, Restore: &ipc.RestoreData{Done: n, Total: total, Bytes: bytes}})
		},
	}
	ts.logf("%s : restauration de %d fichier(s) de %s (%s)", ts.u.Account(), len(chosen), l.path, mode)
	execute(ctx, io.Discard, ts.sd.logw, d, ts.cfg, ts.b, p, true, []string{l.path, "--from-tray", rd.Mode})
	info := p.info
	ts.logf("%s : %s, %d restauré(s), %d en échec, journal %s", ts.u.Account(), info.Result, info.Files, info.Failed, info.Log)
	r := ipc.RestoreData{Done: info.Files, Bytes: info.Bytes, Failed: info.Failed, Kept: len(kept), Dir: where, Log: info.Log}
	switch {
	case info.Result == "interrupted":
		reply(r, "Restauration interrompue.", false)
	case info.Ignored > 0:
		reply(r, fmt.Sprintf("%d fichier(s) au nom refusé (.., caractères interdits) non restauré(s).", info.Ignored), info.Failed == 0)
	default:
		done(r, "")
	}
}
