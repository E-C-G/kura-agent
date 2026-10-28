package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/report"
	"github.com/E-C-G/kura-agent/internal/restore"
	"github.com/E-C-G/kura-agent/pkg/config"
)

type toBench struct {
	dir     string // racine du test
	src     string // source du job home
	l       *fakeLister
	d       restoreDeps
	reports []report.RestoreInfo
	free    uint64
	answer  bool
	busy    bool // sauvegarde en cours : verrou refusé
	locked  int  // verrou pris
	held    bool // verrou tenu en ce moment
}

func newToBench(t *testing.T) *toBench {
	t.Helper()
	b := &toBench{dir: t.TempDir(), free: 1 << 40}
	b.src = filepath.Join(b.dir, "home", "alice")
	projets := filepath.Join(b.dir, "srv", "projets")
	at := func(d int) time.Time { return restoreNow.AddDate(0, 0, d) }
	b.l = &fakeLister{versions: []restore.Version{
		{Key: "acme-srv01/home/Documents/a.txt", VersionID: "a1", LastModified: at(-20), Size: 10},
		{Key: "acme-srv01/home/Documents/a.txt", VersionID: "a2", LastModified: at(-2), Size: 12, Latest: true},
		{Key: "acme-srv01/home/Documents/sous dossier/b.txt", VersionID: "b1", LastModified: at(-10), Size: 5, Latest: true},
		{Key: "acme-srv01/home/Documents/supprimé.txt", VersionID: "d1", LastModified: at(-30), Size: 7},
		{Key: "acme-srv01/home/Documents/supprimé.txt", VersionID: "dx", LastModified: at(-1), Deleted: true, Latest: true},
		{Key: "acme-srv01/home/Documents/../évasion.txt", VersionID: "e1", LastModified: at(-3), Size: 3, Latest: true},
		{Key: "acme-srv01/projets/Plan.odt", VersionID: "p1", LastModified: at(-5), Size: 20, Latest: true},
	}}
	cfg := &config.Config{Jobs: []config.Job{
		{ID: "home", Source: b.src, Destination: "acme-srv01/home"},
		{ID: "projets", Source: projets, Destination: "acme-srv01/projets"},
	}}
	b.d = restoreDeps{
		checkAdmin:  func() error { return nil },
		loadConfig:  func(string) (*config.Config, error) { return cfg, nil },
		bucket:      func(*config.Config) (restoreBucket, error) { return b.l, nil },
		now:         func() time.Time { return restoreNow },
		windows:     runtime.GOOS == "windows",
		foldCase:    runtime.GOOS == "windows" || runtime.GOOS == "darwin",
		interactive: true,
		confirm:     func(string) (bool, error) { return b.answer, nil },
		freeSpace:   func(string) (uint64, error) { return b.free, nil },
		owner:       destOwner,
		logsDir:     filepath.Join(b.dir, "logs"),
		statusFile:  filepath.Join(b.dir, "data", "restore.json"),
		account:     func() string { return "admin" },
		report: func(_ context.Context, _ *config.Config, info report.RestoreInfo) error {
			b.reports = append(b.reports, info)
			return nil
		},
		workers: 3,
		version: "test",
		lock: func() (func(), error) {
			if b.busy {
				return nil, lock.ErrBusy
			}
			b.locked++
			b.held = true
			return func() { b.held = false }, nil
		},
		inPlaceOwner: inPlaceOwner,
	}
	return b
}

func (b *toBench) run(args ...string) (int, string, string) { return restoreRun(b.d, args...) }

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			data, _ := os.ReadFile(p)
			out[filepath.ToSlash(rel)] = string(data)
		}
		return nil
	})
	return out
}

func want(v string, size int) string {
	return string(content(restore.Version{VersionID: v, Size: int64(size)}))
}

func TestRestoreToFolder(t *testing.T) {
	b := newToBench(t)
	to := filepath.Join(b.dir, "restauration", "R1") // parents absents : créés
	code, out, stderr := b.run(filepath.Join(b.src, "Documents"), "--to", to, "--yes")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, stderr)
	}
	got := readTree(t, to)
	exp := map[string]string{"Documents/a.txt": want("a2", 12), "Documents/sous dossier/b.txt": want("b1", 5)}
	if len(got) != len(exp) {
		t.Fatalf("%v", got)
	}
	for k, v := range exp {
		if got[k] != v {
			t.Errorf("%s : %q", k, got[k])
		}
	}
	fi, _ := os.Stat(filepath.Join(to, "Documents", "a.txt"))
	if !fi.ModTime().Equal(restoreNow.AddDate(0, 0, -2)) {
		t.Errorf("date %v", fi.ModTime())
	}
	if !strings.Contains(out, "2 fichier(s) restauré(s)") || !strings.Contains(out, "1 nom(s) refusé(s)") || !strings.Contains(out, "Journal : ") {
		t.Errorf("%s", out)
	}
	// journal, état, bilan
	logs, _ := filepath.Glob(filepath.Join(b.dir, "logs", "restore-*.log"))
	if len(logs) != 1 {
		t.Fatalf("journaux : %v", logs)
	}
	lj, _ := os.ReadFile(logs[0])
	for _, s := range []string{"OK\t" + filepath.FromSlash("Documents/a.txt") + "\tversion a2", "REFUSÉ\t", "Compte : admin", "Commande : kura-agent restore", "Résumé : 2 restauré(s)"} {
		if !strings.Contains(string(lj), s) {
			t.Errorf("journal sans %q :\n%s", s, lj)
		}
	}
	var st report.RestoreInfo
	data, _ := os.ReadFile(b.d.statusFile)
	if err := json.Unmarshal(data, &st); err != nil || st.Files != 2 || st.Bytes != 17 || st.Ignored != 1 || st.Result != "ok" || st.Log != logs[0] {
		t.Fatalf("%+v %v", st, err)
	}
	if len(b.reports) != 1 || b.reports[0].Files != 2 || b.reports[0].At != nil {
		t.Fatalf("bilan : %+v", b.reports)
	}
	var buf bytes.Buffer
	printRestoreStatus(&buf, b.d.statusFile)
	if !strings.Contains(buf.String(), "Dernière restauration") || !strings.Contains(buf.String(), "2 fichier(s)") {
		t.Errorf("status : %s", buf.String())
	}
	// reprise effacée : la même commande refuse désormais (dossier non vide)
	if code, _, stderr := b.run(filepath.Join(b.src, "Documents"), "--to", to, "--yes"); code != exitRestoreRefused || !strings.Contains(stderr, "pas vide") {
		t.Fatalf("relance : %d %q", code, stderr)
	}
}

func TestRestoreToAtDateFileAndAll(t *testing.T) {
	b := newToBench(t)
	to := filepath.Join(b.dir, "R")
	code, _, stderr := b.run(filepath.Join(b.src, "Documents"), "--at", restoreNow.AddDate(0, 0, -15).Format("2006-01-02"), "--to", to, "--yes")
	if code != 0 {
		t.Fatal(code, stderr)
	}
	got := readTree(t, to)
	if got["Documents/a.txt"] != want("a1", 10) || got["Documents/supprimé.txt"] != want("d1", 7) || len(got) != 2 {
		// b.txt n'existait pas encore (sauvegardé à j-10)
		t.Fatalf("%v", got)
	}
	if b.reports[0].At == nil {
		t.Error("date absente du bilan")
	}

	// un fichier : il arrive directement dans --to
	to2 := filepath.Join(b.dir, "R2")
	if code, _, stderr := b.run(filepath.Join(b.src, "Documents", "a.txt"), "--to", to2, "--yes"); code != 0 {
		t.Fatal(code, stderr)
	}
	if got := readTree(t, to2); len(got) != 1 || got["a.txt"] != want("a2", 12) {
		t.Fatalf("%v", got)
	}

	// --all : un sous-dossier par job
	to3 := filepath.Join(b.dir, "R3")
	if code, _, stderr := b.run("--all", "--to", to3, "--yes"); code != 0 {
		t.Fatal(code, stderr)
	}
	got = readTree(t, to3)
	if got["home/Documents/a.txt"] != want("a2", 12) || got["projets/Plan.odt"] != want("p1", 20) || len(got) != 3 {
		t.Fatalf("%v", got)
	}
}

func TestRestoreToRefusals(t *testing.T) {
	b := newToBench(t)
	docs := filepath.Join(b.dir, "home", "alice", "Documents")
	cases := []struct {
		name, to, msg string
		setup         func(string)
	}{
		{"dans une source", filepath.Join(b.src, "restauration"), "source du job home", nil},
		{"non vide", filepath.Join(b.dir, "plein"), "pas vide", func(p string) { os.MkdirAll(p, 0o755); os.WriteFile(filepath.Join(p, "x"), nil, 0o644) }},
		{"fichier", filepath.Join(b.dir, "fichier"), "est un fichier", func(p string) { os.WriteFile(p, nil, 0o644) }},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			name, to, msg string
			setup         func(string)
		}{"lien", filepath.Join(b.dir, "lien"), "lien symbolique", func(p string) { os.Symlink(t.TempDir(), p) }})
	}
	for _, c := range cases {
		if c.setup != nil {
			c.setup(c.to)
		}
		code, _, stderr := b.run(docs, "--to", c.to, "--yes")
		if code != exitRestoreRefused || !strings.Contains(stderr, c.msg) {
			t.Errorf("%s : %d %q", c.name, code, stderr)
		}
	}
	if len(b.l.gets) != 0 || len(b.l.prefixes) != 0 {
		t.Fatalf("bucket lu malgré le refus : %v %v", b.l.prefixes, b.l.gets)
	}

	to := filepath.Join(b.dir, "R")
	b.free = 10
	if code, _, stderr := b.run(docs, "--to", to, "--yes"); code != exitRestoreRefused || !strings.Contains(stderr, "libres") {
		t.Errorf("espace : %d %q", code, stderr)
	}
	b.free = 1 << 40
	b.d.interactive = false
	if code, _, stderr := b.run(docs, "--to", to); code != exitRestoreRefused || !strings.Contains(stderr, "--yes") {
		t.Errorf("sans terminal : %d %q", code, stderr)
	}
	b.d.interactive = true
	b.answer = false
	if code, _, stderr := b.run(docs, "--to", to); code != exitRestoreRefused || !strings.Contains(stderr, "non confirmée") {
		t.Errorf("non confirmé : %d %q", code, stderr)
	}
	if _, err := os.Stat(to); !errors.Is(err, os.ErrNotExist) || len(b.l.gets) != 0 {
		t.Fatalf("écrit sans confirmation : %v %v", err, b.l.gets)
	}
	b.answer = true
	if code, _, _ := b.run(docs, "--to", to); code != 0 {
		t.Fatal("confirmé :", code)
	}
	// rien à restaurer : date avant la première sauvegarde
	if code, out, _ := b.run(docs, "--to", filepath.Join(b.dir, "R4"), "--at", "2026-01-01", "--yes"); code != 0 || !strings.Contains(out, "Rien à restaurer") {
		t.Errorf("rien : %d %q", code, out)
	}
}

func TestRestoreToResume(t *testing.T) {
	b := newToBench(t)
	docs := filepath.Join(b.src, "Documents")
	to := filepath.Join(b.dir, "R")
	b.l.getErr = map[string]error{"b1": errors.New("connexion coupée")}
	b.l.corrupt = map[string]bool{"a2": true}
	code, out, _ := b.run(docs, "--to", to, "--yes")
	if code != exitRestorePartial || !strings.Contains(out, "2 en échec") || !strings.Contains(out, "altérée") || !strings.Contains(out, "connexion coupée") {
		t.Fatalf("%d %s", code, out)
	}
	if b.reports[0].Result != "partial" || b.reports[0].Failed != 2 {
		t.Fatalf("%+v", b.reports[0])
	}
	if _, err := os.Stat(filepath.Join(to, restore.MarkerName)); err != nil {
		t.Fatal("reprise absente après un échec")
	}

	// une autre restauration vers ce dossier est refusée
	if code, _, stderr := b.run(docs, "--to", to, "--at", "2026-10-20", "--yes"); code != exitRestoreRefused || !strings.Contains(stderr, "autre restauration") {
		t.Fatalf("autre : %d %q", code, stderr)
	}

	// a2 seul réussit, puis tout : la reprise saute ce qui est fait
	b.l.corrupt = nil
	if code, _, _ := b.run(docs, "--to", to, "--yes"); code != exitRestorePartial {
		t.Fatal(code)
	}
	b.l.getErr = nil
	b.l.gets = nil
	code, out, _ = b.run(docs, "--to", to, "--yes")
	if code != 0 || !strings.Contains(out, "1 déjà restauré(s)") || len(b.l.gets) != 1 || b.l.gets[0] != "b1" {
		t.Fatalf("%d %s %v", code, out, b.l.gets)
	}
	got := readTree(t, to)
	if len(got) != 2 || got["Documents/a.txt"] != want("a2", 12) {
		t.Fatalf("%v", got)
	}
}

func TestRestoreToWindowsDisplay(t *testing.T) {
	if dispPath("C:/Users/alice/Documents", true) != `C:\Users\alice\Documents` || dispPath("/home/alice", false) != "/home/alice" {
		t.Fatal("dispPath")
	}
	tg := restore.Target{Job: config.Job{ID: "racine"}, Local: "D:"}
	if outName(tg, false) != "racine" || outName(restore.Target{Local: "C:/Users/alice/Documents"}, false) != "Documents" {
		t.Fatal("outName")
	}
	if quoteArgs([]string{`C:\a b`, "--yes"}) != `"C:\a b" --yes` {
		t.Fatal(quoteArgs([]string{`C:\a b`, "--yes"}))
	}
}
