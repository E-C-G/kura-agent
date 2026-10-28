package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/E-C-G/kura-agent/internal/ipc"
	"github.com/E-C-G/kura-agent/internal/lock"
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
	// CanWrite : le compte peut créer des fichiers dans dir (contrôle fait par le
	// système avec le jeton du compte).
	CanWrite(dir string) error
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
	versions []restore.Version
	files    []restore.File
}

func (ts *traySession) serve(ctx context.Context, conn io.ReadWriteCloser) {
	msgs := make(chan ipc.Msg, 64)
	peer := ipc.NewPeer(conn, func(m ipc.Msg) {
		select {
		case msgs <- m:
		default: // fenêtre trop bavarde : ignoré
		}
	})
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
		if restore.CheckRel(rd.Rel) != "" {
			fail(ipc.TypeRestoreVersions, "Fichier inconnu.")
			return
		}
		key := rd.Rel
		if k := ts.listing.target.Key; k != "" {
			key = k + "/" + rd.Rel
		}
		var vs []ipc.RestoreVersion
		for _, v := range restore.History(ts.listing.versions, key, ts.sd.windows) {
			if len(vs) == trayMaxVersions {
				break
			}
			vs = append(vs, ipc.RestoreVersion{Date: v.LastModified, Size: v.Size, Deleted: v.Deleted, Current: v.Latest})
		}
		reply(ipc.Msg{Type: ipc.TypeRestoreVersions, Restore: &ipc.RestoreData{Rel: rd.Rel, Versions: vs}})
	case ipc.TypeRestorePick:
		if ts.picks == nil {
			ts.picks = map[string]bool{}
		}
		for _, r := range rd.Rels {
			ts.picks[r] = true
		}
	case ipc.TypeRestoreRun:
		ts.run(ctx, peer, m, rd)
	}
}

// list : état du dossier choisi à la date demandée.
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
	t, _ := restore.Resolve(ts.cfg, dir, ts.sd.windows)
	var at time.Time
	if rd.At != nil && rd.At.Before(ts.sd.now()) {
		at = *rd.At
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	vs, err := listTarget(lctx, ts.b, t, ts.sd.windows)
	if err != nil {
		ts.logf("%s : liste de %s : %v", ts.u.Account(), dir, err)
		fail("Bucket injoignable : %v", err)
		return
	}
	files := restore.StateAt(vs, t, at, ts.sd.windows)
	msg := ""
	if len(files) > trayMaxFiles {
		files = files[:trayMaxFiles]
		msg = fmt.Sprintf("Plus de %d fichiers : seuls les %d premiers sont listés ; choisir un sous-dossier.", trayMaxFiles, trayMaxFiles)
	}
	ts.listing = &trayListing{path: dir, target: t, at: at, versions: vs, files: files}
	var out []ipc.RestoreFile
	var total int64
	for _, f := range files {
		out = append(out, ipc.RestoreFile{Rel: f.Rel, Size: f.Version.Size, BackedUp: f.Version.LastModified,
			DeletedSince: f.DeletedSince, Newer: f.NewerSince, Problem: f.Problem})
		total += f.Version.Size
	}
	for _, chunk := range ipc.ChunkFiles(out) {
		if err := peer.Send(ipc.Msg{Type: ipc.TypeRestoreFiles, Restore: &ipc.RestoreData{Req: m.ID, Files: chunk}}); err != nil {
			return
		}
	}
	_ = peer.Reply(m, ipc.Msg{Type: ipc.TypeRestoreListed, Path: dir, Message: msg,
		Restore: &ipc.RestoreData{Total: len(files), Bytes: total}})
}

// allowed vérifie un dossier demandé par la fenêtre : absolu, dans la source d'un job,
// et le compte peut écrire dans son plus proche dossier existant.
func (ts *traySession) allowed(path string) (string, error) {
	if path == "" {
		return "", errors.New("Choisir un dossier.")
	}
	dir := filepath.Clean(path)
	if !filepath.IsAbs(dir) && !(ts.sd.windows && len(dir) >= 3 && dir[1] == ':') {
		return "", errors.New("Chemin incomplet : choisir un dossier.")
	}
	if _, err := restore.Resolve(ts.cfg, dir, ts.sd.windows); err != nil {
		return "", fmt.Errorf("%s n'est pas sauvegardé par Kura.", dir)
	}
	if err := ts.u.CanWrite(nearestExisting(dir)); err != nil {
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
	// Le verrou de la sauvegarde (à leur place) est rendu avant la réponse.
	unlock := func() {}
	reply := func(r ipc.RestoreData, msg string, ok bool) {
		unlock()
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
	// Le dossier est revérifié : les droits ont pu changer depuis la liste.
	if _, err := ts.allowed(l.path); err != nil {
		done(ipc.RestoreData{}, err.Error())
		return
	}
	d := ts.sd.restoreDeps
	disp := func(p string) string { return dispPath(p, d.windows) }
	var root, where, mode string
	var items []restore.Item
	var kept, preFailed []keptItem
	switch rd.Mode {
	case ipc.RestoreCopy:
		parent, base := filepath.Dir(l.path), filepath.Base(l.path)
		if parent == l.path || restore.CheckRel(base) != "" {
			parent, base = l.path, "" // racine d'un lecteur : le nouveau dossier va dedans
		}
		if err := ts.u.CanWrite(parent); err != nil {
			done(ipc.RestoreData{}, fmt.Sprintf("Vous ne pouvez pas créer de dossier dans %s : choisir « À leur place ».", parent))
			return
		}
		name := "Restauration Kura " + ts.sd.now().Format("2006-01-02 15h04")
		for i := 2; ; i++ {
			if _, err := os.Lstat(filepath.Join(parent, name)); errors.Is(err, fs.ErrNotExist) {
				break
			}
			name = fmt.Sprintf("Restauration Kura %s (%d)", ts.sd.now().Format("2006-01-02 15h04"), i)
		}
		root, where, mode = parent, filepath.Join(parent, name), "to"
		for _, f := range chosen {
			out := name + "/" + f.Rel
			if base != "" {
				out = name + "/" + base + "/" + f.Rel
			}
			items = append(items, restore.Item{File: f, Out: out})
		}
	case ipc.RestoreMissing:
		root, where, mode = l.path, l.path, "in_place"
		for _, f := range chosen {
			items = append(items, restore.Item{File: f, Out: f.Rel})
		}
	default:
		done(ipc.RestoreData{}, "Mode inconnu.")
		return
	}
	restore.MarkConflicts(items, d.foldCase)

	if mode == "in_place" {
		release, err := d.lock()
		if err != nil {
			msg := fmt.Sprintf("Verrou de la sauvegarde : %v", err)
			if errors.Is(err, lock.ErrBusy) {
				msg = "Une sauvegarde est en cours : réessayer quand elle sera finie."
			}
			done(ipc.RestoreData{}, msg)
			return
		}
		var once sync.Once
		unlock = func() { once.Do(release) }
		defer unlock()
	}

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
