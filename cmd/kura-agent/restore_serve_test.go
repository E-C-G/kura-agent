package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/ipc"
	"github.com/E-C-G/kura-agent/internal/restore"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

type fakeTrayUser struct {
	account    string
	home       string
	allowed    []string
	denied     []string
	unreadable []string
	mu         sync.Mutex
	imp        int // emprunts d'identité
	closed     bool
}

func (u *fakeTrayUser) Account() string { return u.account }
func (u *fakeTrayUser) Home() string    { return u.home }
func (u *fakeTrayUser) Close()          { u.closed = true }
func (u *fakeTrayUser) Do(f func() error) error {
	undo, _ := u.Impersonate()
	defer undo()
	return f()
}
func (u *fakeTrayUser) Impersonate() (func(), error) {
	u.mu.Lock()
	u.imp++
	u.mu.Unlock()
	return func() {}, nil
}
func (u *fakeTrayUser) CanReadAll(files []string) []error {
	out := make([]error, len(files))
	for i, f := range files {
		for _, x := range u.unreadable {
			if f == x {
				out[i] = errors.New("accès refusé")
			}
		}
	}
	return out
}
func (u *fakeTrayUser) CanWriteAll(dirs []string) []error {
	out := make([]error, len(dirs))
	for i, d := range dirs {
		out[i] = u.CanWrite(d)
	}
	return out
}
func (u *fakeTrayUser) CanWrite(dir string) error {
	for _, a := range u.denied {
		if dir == a || strings.HasPrefix(dir, a+string(filepath.Separator)) {
			return errors.New("accès refusé")
		}
	}
	for _, a := range u.allowed {
		if dir == a || strings.HasPrefix(dir, a+string(filepath.Separator)) {
			return nil
		}
	}
	return errors.New("accès refusé")
}

// window : la fenêtre de l'icône, simulée au bout du canal.
type window struct {
	peer  *ipc.Peer
	mu    sync.Mutex
	hello chan ipc.Msg
	files map[int][]ipc.RestoreFile
	prog  []ipc.RestoreData
}

func (w *window) request(t *testing.T, m ipc.Msg) ipc.Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := w.peer.Request(ctx, m)
	if err != nil {
		t.Fatalf("%s : %v", m.Type, err)
	}
	return r
}

func startServe(t *testing.T, b *toBench, u *fakeTrayUser, opts ...func(*serveDeps)) (*window, chan int) {
	t.Helper()
	server, client := net.Pipe()
	w := &window{hello: make(chan ipc.Msg, 1), files: map[int][]ipc.RestoreFile{}}
	w.peer = ipc.NewPeer(client, func(m ipc.Msg) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch m.Type {
		case ipc.TypeRestoreHello:
			w.hello <- m
		case ipc.TypeRestoreFiles:
			w.files[m.Restore.Req] = append(w.files[m.Restore.Req], m.Restore.Files...)
		case ipc.TypeRestoreProgress:
			w.prog = append(w.prog, *m.Restore)
		}
	})
	var once sync.Once
	sd := serveDeps{
		restoreDeps: b.d,
		sessions:    func() ([]int, error) { return []int{1}, nil },
		dial: func(int) (io.ReadWriteCloser, trayUser, error) {
			var c io.ReadWriteCloser
			once.Do(func() { c = server })
			if c == nil {
				return nil, nil, errors.New("pas de fenêtre")
			}
			return c, u, nil
		},
		poll: 10 * time.Millisecond, idle: time.Minute, linger: 50 * time.Millisecond, logw: io.Discard,
	}
	for _, o := range opts {
		o(&sd)
	}
	code := make(chan int, 1)
	go func() { code <- serveRestore(context.Background(), sd, "") }()
	return w, code
}

func TestServeRestoreFromTray(t *testing.T) {
	b := newToBench(t)
	cfg, _ := b.d.loadConfig("")
	cfg.Tray.Users = []string{"alice"}
	os.MkdirAll(b.src, 0o755)
	u := &fakeTrayUser{account: `ACME\alice`, home: b.src, allowed: []string{b.src}}
	w, code := startServe(t, b, u)

	h := <-w.hello
	if h.Message != "" || h.Restore.Account != `ACME\alice` || len(h.Restore.Roots) != 1 || h.Restore.Roots[0] != b.src || h.Restore.Days != 90 {
		t.Fatalf("hello : %+v %+v", h, h.Restore)
	}
	docs := filepath.Join(b.src, "Documents")

	// liste du dossier : par lots, puis le total
	m := ipc.Msg{Type: ipc.TypeRestoreList, Path: docs, Restore: &ipc.RestoreData{}}
	r := w.request(t, m)
	if r.Message != "" || r.Restore.Total != 3 || r.Restore.Bytes != 20 {
		t.Fatalf("liste : %q %+v", r.Message, r.Restore)
	}
	w.mu.Lock()
	var got []ipc.RestoreFile
	for _, fs := range w.files {
		got = append(got, fs...)
	}
	w.mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("fichiers reçus : %+v", got)
	}

	// hors de son périmètre : refusé, sans lecture du bucket
	before := len(b.l.prefixes)
	os.MkdirAll(filepath.Join(b.dir, "srv", "projets"), 0o755)
	r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreList, Path: filepath.Join(b.dir, "srv", "projets"), Restore: &ipc.RestoreData{}})
	if !strings.Contains(r.Message, "pas le droit") || len(b.l.prefixes) != before {
		t.Fatalf("projets : %q", r.Message)
	}
	r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreList, Path: filepath.Join(b.dir, "ailleurs"), Restore: &ipc.RestoreData{}})
	if !strings.Contains(r.Message, "pas sauvegardé") {
		t.Fatalf("ailleurs : %q", r.Message)
	}

	// relister le bon dossier, historique, puis restauration dans un nouveau dossier
	w.request(t, m)
	r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreHistory, Restore: &ipc.RestoreData{Rel: "a.txt"}})
	if len(r.Restore.Versions) != 2 || !r.Restore.Versions[0].Current {
		t.Fatalf("historique : %+v", r.Restore)
	}
	w.peer.Send(ipc.Msg{Type: ipc.TypeRestorePick, Restore: &ipc.RestoreData{Rels: []string{"a.txt", "../évasion.txt"}}})
	w.peer.Send(ipc.Msg{Type: ipc.TypeRestorePick, Restore: &ipc.RestoreData{Rels: []string{"sous dossier/b.txt"}}})
	r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreRun, Restore: &ipc.RestoreData{Mode: ipc.RestoreCopy}})
	wantDir := filepath.Join(b.src, "Restauration Kura 2026-10-28 10h00")
	if !r.OK || r.Restore.Done != 2 || r.Restore.Dir != wantDir || !strings.Contains(r.Message, "1 fichier(s) au nom refusé") {
		t.Fatalf("restauration : ok=%v %q %+v", r.OK, r.Message, r.Restore)
	}
	if data, err := os.ReadFile(filepath.Join(wantDir, "Documents", "a.txt")); err != nil || string(data) != want("a2", 12) {
		t.Fatalf("fichier restauré : %q %v", data, err)
	}
	if u.imp < 2 {
		t.Errorf("écriture sans emprunt d'identité (%d)", u.imp)
	}
	if len(w.prog) == 0 || w.prog[len(w.prog)-1].Done != w.prog[len(w.prog)-1].Total {
		t.Errorf("avancement : %+v", w.prog)
	}
	st := b.reports[len(b.reports)-1]
	if st.Account != `ACME\alice` || st.Mode != "to" || st.Files != 2 {
		t.Errorf("bilan : %+v", st)
	}

	// à leur place : seulement les absents, sans verrou de la sauvegarde
	os.MkdirAll(docs, 0o755)
	os.WriteFile(filepath.Join(docs, "a.txt"), []byte("actuel"), 0o644)
	w.request(t, m)
	w.peer.Send(ipc.Msg{Type: ipc.TypeRestorePick, Restore: &ipc.RestoreData{Rels: []string{"a.txt", "sous dossier/b.txt"}}})
	r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreRun, Restore: &ipc.RestoreData{Mode: ipc.RestoreMissing}})
	if !r.OK || r.Restore.Done != 1 || r.Restore.Kept != 1 || b.locked != 0 {
		t.Fatalf("à leur place : ok=%v %q %+v verrou %d", r.OK, r.Message, r.Restore, b.locked)
	}
	if data, _ := os.ReadFile(filepath.Join(docs, "a.txt")); string(data) != "actuel" {
		t.Fatal("fichier présent remplacé")
	}
	if data, _ := os.ReadFile(filepath.Join(docs, "sous dossier", "b.txt")); string(data) != want("b1", 5) {
		t.Fatal("fichier absent non remis")
	}
	// au-delà de 90 jours : refusé par la tâche, quoi que demande la fenêtre
	old := restoreNow.AddDate(0, 0, -120)
	if r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreList, Path: docs, Restore: &ipc.RestoreData{At: &old}}); !strings.Contains(r.Message, "90 jours") {
		t.Fatalf("120 jours : %q", r.Message)
	}
	// chemin piégé
	if r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreList, Path: docs + "\nx", Restore: &ipc.RestoreData{}}); r.Message != "Chemin invalide." {
		t.Fatalf("caractère de contrôle : %q", r.Message)
	}

	// fenêtre fermée : la tâche s'arrête
	w.peer.Close()
	select {
	case c := <-code:
		if c != 0 || !u.closed {
			t.Fatalf("code %d, jeton fermé %v", c, u.closed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("la tâche ne s'arrête pas")
	}
}

func TestServeRefusesUnlistedAccount(t *testing.T) {
	b := newToBench(t)
	cfg, _ := b.d.loadConfig("")
	cfg.Tray.Users = []string{"bob"}
	_ = config.Listed
	w, code := startServe(t, b, &fakeTrayUser{account: `ACME\alice`, allowed: []string{b.src}})
	h := <-w.hello
	if !strings.Contains(h.Message, "tray.users") || len(h.Restore.Roots) != 0 {
		t.Fatalf("%+v", h)
	}
	select {
	case <-w.peer.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("connexion gardée")
	}
	<-code
}

// Dossier commun : un sous-dossier privé n'apparaît pas, un dossier disparu hors du
// profil n'est pas restaurable depuis l'icône.
func TestServeScopeSharedFolder(t *testing.T) {
	b := newToBench(t)
	cfg, _ := b.d.loadConfig("")
	cfg.Tray.Users = []string{"alice"}
	docs := filepath.Join(b.src, "Documents")
	private := filepath.Join(docs, "sous dossier")
	os.MkdirAll(private, 0o755)
	os.WriteFile(filepath.Join(docs, "a.txt"), []byte("actuel"), 0o644)
	os.WriteFile(filepath.Join(private, "b.txt"), []byte("privé"), 0o644)
	u := &fakeTrayUser{account: `ACME\alice`, allowed: []string{b.src}, denied: []string{private}}
	w, code := startServe(t, b, u)
	<-w.hello

	m := ipc.Msg{Type: ipc.TypeRestoreList, Path: docs, Restore: &ipc.RestoreData{Req: 7}}
	r := w.request(t, m)
	if r.Restore.Total != 1 || !strings.Contains(r.Message, "pas accessibles") || r.Restore.Req != 7 {
		t.Fatalf("liste : %q %+v", r.Message, r.Restore)
	}
	w.mu.Lock()
	for _, f := range w.files[7] {
		if strings.HasPrefix(f.Rel, "sous dossier") {
			t.Errorf("fichier privé listé : %s", f.Rel)
		}
	}
	w.mu.Unlock()
	if r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreHistory, Restore: &ipc.RestoreData{Rel: "sous dossier/b.txt"}}); len(r.Restore.Versions) != 0 {
		t.Fatal("historique d'un fichier privé")
	}
	w.peer.Send(ipc.Msg{Type: ipc.TypeRestorePick, Restore: &ipc.RestoreData{Rels: []string{"sous dossier/b.txt"}}})
	if r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreRun, Restore: &ipc.RestoreData{Mode: ipc.RestoreCopy}}); r.OK || r.Message != "Aucun fichier coché." {
		t.Fatalf("restauration d'un fichier privé : %+v", r)
	}
	// dossier disparu hors du profil : refusé sans lecture du bucket
	n := len(b.l.prefixes)
	if r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreList, Path: filepath.Join(b.src, "Disparu"), Restore: &ipc.RestoreData{}}); !strings.Contains(r.Message, "n'existe plus") || len(b.l.prefixes) != n {
		t.Fatalf("dossier disparu : %q", r.Message)
	}
	w.peer.Close()
	<-code
}

// Hors du profil : seuls les fichiers présents et lisibles ; dans le profil, aussi les
// fichiers supprimés.
func TestServeScopeReadAndDeleted(t *testing.T) {
	b := newToBench(t)
	cfg, _ := b.d.loadConfig("")
	cfg.Tray.Users = []string{"alice"}
	docs := filepath.Join(b.src, "Documents")
	os.MkdirAll(filepath.Join(docs, "sous dossier"), 0o755)
	a := filepath.Join(docs, "a.txt")
	os.WriteFile(a, []byte("actuel"), 0o644)
	os.WriteFile(filepath.Join(docs, "sous dossier", "b.txt"), []byte("actuel"), 0o644)
	list := func(u *fakeTrayUser, req int) []string {
		w, code := startServe(t, b, u)
		<-w.hello
		r := w.request(t, ipc.Msg{Type: ipc.TypeRestoreList, Path: docs, Restore: &ipc.RestoreData{Req: req}})
		w.mu.Lock()
		var rels []string
		for _, f := range w.files[req] {
			rels = append(rels, f.Rel)
		}
		w.mu.Unlock()
		if r.Restore.Total != len(rels) {
			t.Fatalf("total %d, reçus %v", r.Restore.Total, rels)
		}
		w.peer.Close()
		<-code
		return rels
	}
	// dossier commun (pas de profil) : a.txt illisible masqué, évasion (supprimé) masqué
	if rels := list(&fakeTrayUser{account: `ACME\alice`, allowed: []string{b.src}, unreadable: []string{a}}, 3); strings.Join(rels, ",") != "sous dossier/b.txt" {
		t.Fatalf("hors du profil : %v", rels)
	}
	// profil du compte : les fichiers supprimés apparaissent aussi
	if rels := list(&fakeTrayUser{account: `ACME\alice`, home: b.src, allowed: []string{b.src}, unreadable: []string{a}}, 4); strings.Join(rels, ",") != "../évasion.txt,sous dossier/b.txt" {
		t.Fatalf("profil : %v", rels)
	}
}

// M1 : ce qu'une jonction a fait entrer dans le bucket n'est jamais servi depuis l'icône,
// même « supprimé dans le profil » : quarantaine, fichiers du coffre, dossiers de l'agent.
func TestServeNeverServesAgentSecrets(t *testing.T) {
	b := newToBench(t)
	cfg, _ := b.d.loadConfig("")
	cfg.Tray.Users = []string{"alice"}
	at := restoreNow.AddDate(0, 0, -3)
	for _, k := range []string{"j/wasabi.secret", "j/notes.txt", "k/hc-url.secret", "kit/acme.kura", "agent/status.json"} {
		b.l.versions = append(b.l.versions,
			restore.Version{Key: "acme-srv01/home/Documents/" + k, VersionID: k, LastModified: at, Size: 4},
			restore.Version{Key: "acme-srv01/home/Documents/" + k, VersionID: k + "x", LastModified: at.Add(time.Hour), Deleted: true, Latest: true})
	}
	docs := filepath.Join(b.src, "Documents")
	os.MkdirAll(docs, 0o755)
	u := &fakeTrayUser{account: `ACME\\alice`, home: b.src, allowed: []string{b.src}}
	w, code := startServe(t, b, u, func(sd *serveDeps) {
		sd.quarantine = func() []state.QuarantineEntry {
			return []state.QuarantineEntry{{Prefix: "acme-srv01/home/Documents/j"}}
		}
		sd.agentDirs = []string{filepath.Join(docs, "agent")}
	})
	<-w.hello
	r := w.request(t, ipc.Msg{Type: ipc.TypeRestoreList, Path: docs, Restore: &ipc.RestoreData{At: &at, Req: 5}})
	w.mu.Lock()
	var rels []string
	for _, f := range w.files[5] {
		rels = append(rels, f.Rel)
	}
	w.mu.Unlock()
	for _, rel := range rels {
		if strings.HasPrefix(rel, "j/") || strings.HasSuffix(rel, ".secret") || strings.HasSuffix(rel, ".kura") || strings.HasPrefix(rel, "agent/") {
			t.Errorf("servi : %s", rel)
		}
	}
	if !strings.Contains(r.Message, "pas accessibles") {
		t.Errorf("masquage non signalé : %q", r.Message)
	}
	// un dossier de l'agent ne peut pas être choisi
	if r = w.request(t, ipc.Msg{Type: ipc.TypeRestoreList, Path: filepath.Join(docs, "agent"), Restore: &ipc.RestoreData{}}); !strings.Contains(r.Message, "dossier de Kura") {
		t.Errorf("dossier de l'agent : %q", r.Message)
	}
	w.peer.Close()
	<-code
}
