package restore

import (
	"errors"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/pkg/config"
)

func testConfig() *config.Config {
	return &config.Config{Jobs: []config.Job{
		{ID: "documents", Source: `C:\Users\alice\Documents`, Destination: "acme-portable01/documents"},
		{ID: "projets", Source: `D:/Projets`, Destination: "/acme-portable01/projets/"},
		{ID: "clients", Source: `D:\Projets\Clients`, Destination: "acme-portable01/clients"},
		{ID: "absent", Source: `E:\Archives`, Destination: "acme-portable01/archives", Unavailable: "lecteur absent"},
	}}
}

func TestResolveWindows(t *testing.T) {
	cfg := testConfig()
	cases := []struct {
		local, job, key string
		root            bool
	}{
		{`C:\Users\alice\Documents\Factures\2026.pdf`, "documents", "acme-portable01/documents/Factures/2026.pdf", false},
		{`c:\users\ALICE\documents\factures`, "documents", "acme-portable01/documents/factures", false},
		{`C:\Users\alice\Documents\`, "documents", "acme-portable01/documents", true},
		{`D:\Projets\Plan.docx`, "projets", "acme-portable01/projets/Plan.docx", false},
		// sources imbriquées : la plus précise l'emporte
		{`D:\Projets\Clients\Dupont\devis.xlsx`, "clients", "acme-portable01/clients/Dupont/devis.xlsx", false},
		{`D:\Projets\Clients`, "clients", "acme-portable01/clients", true},
	}
	for _, c := range cases {
		got, err := Resolve(cfg, c.local, true)
		if err != nil {
			t.Fatalf("%s : %v", c.local, err)
		}
		if got.Job.ID != c.job || got.Key != c.key || got.Root != c.root {
			t.Errorf("%s : job %s clé %q racine %v", c.local, got.Job.ID, got.Key, got.Root)
		}
	}
	for _, p := range []string{`D:\ProjetsAnciens\x`, `C:\Users\bob\Documents\a.txt`, `E:\Archives\2020`, ""} {
		if _, err := Resolve(cfg, p, true); err == nil {
			t.Errorf("%q accepté", p)
		}
	}
	if _, err := Resolve(cfg, `D:\ProjetsAnciens\x`, true); !errors.Is(err, ErrOutside) {
		t.Errorf("erreur %v, ErrOutside attendu", err)
	}
}

func TestResolveUnix(t *testing.T) {
	cfg := &config.Config{Jobs: []config.Job{
		{ID: "home", Source: "/home/alice", Destination: "acme-srv01/home"},
		{ID: "racine", Source: "/", Destination: ""},
	}}
	got, err := Resolve(cfg, "/home/alice/notes/a.txt", false)
	if err != nil || got.Job.ID != "home" || got.Key != "acme-srv01/home/notes/a.txt" {
		t.Fatalf("%+v %v", got, err)
	}
	// casse respectée hors Windows : /home/Alice relève de la source /
	got, err = Resolve(cfg, "/home/Alice/a.txt", false)
	if err != nil || got.Job.ID != "racine" || got.Key != "home/Alice/a.txt" {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = Resolve(cfg, "/", false)
	if err != nil || !got.Root || got.Prefix() != "" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestCovers(t *testing.T) {
	file := Target{Key: "d/a.txt"}
	dir := Target{Key: "d/sub"}
	root := Target{Key: "d", Root: true}
	cases := []struct {
		t    Target
		key  string
		fold bool
		want bool
	}{
		{file, "d/a.txt", false, true},
		{file, "d/a.txt.bak", false, false},
		{file, "D/A.TXT", false, false},
		{file, "D/A.TXT", true, true},
		{dir, "d/sub/x", false, true},
		{dir, "d/subway/x", false, false},
		{dir, "d/sub", false, true},
		{root, "d/x", false, true},
		{root, "d", false, false},
		{root, "dx/y", false, false},
		{Target{Root: true}, "n/importe/quoi", false, true},
	}
	for _, c := range cases {
		if got := c.t.Covers(c.key, c.fold); got != c.want {
			t.Errorf("%q couvre %q (casse ignorée %v) = %v", c.t.Key, c.key, c.fold, got)
		}
	}
}

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func day(n int) time.Time { return t0.AddDate(0, 0, n) }

// Historique type sous d/ :
//   - a.txt : créé j0, modifié j10 (version actuelle)
//   - b.txt : créé j0, supprimé j5
//   - c.txt : créé j0, supprimé j3, recréé j8
//   - e.txt : créé j20
//   - sub/f.txt : créé j2, jamais modifié
func history() []Version {
	return []Version{
		{Key: "d/a.txt", VersionID: "a1", LastModified: day(0), Size: 10},
		{Key: "d/a.txt", VersionID: "a2", LastModified: day(10), Size: 12, Latest: true},
		{Key: "d/b.txt", VersionID: "b1", LastModified: day(0), Size: 20},
		{Key: "d/b.txt", VersionID: "bx", LastModified: day(5), Deleted: true, Latest: true},
		{Key: "d/c.txt", VersionID: "c1", LastModified: day(0), Size: 30},
		{Key: "d/c.txt", VersionID: "cx", LastModified: day(3), Deleted: true},
		{Key: "d/c.txt", VersionID: "c2", LastModified: day(8), Size: 31, Latest: true},
		{Key: "d/e.txt", VersionID: "e1", LastModified: day(20), Size: 40, Latest: true},
		{Key: "d/sub/f.txt", VersionID: "f1", LastModified: day(2), Size: 50, Latest: true},
		{Key: "autre/z.txt", VersionID: "z1", LastModified: day(0), Size: 99, Latest: true},
	}
}

func ids(fs []File) map[string]string {
	m := map[string]string{}
	for _, f := range fs {
		m[f.Rel] = f.Version.VersionID
	}
	return m
}

func TestStateAt(t *testing.T) {
	root := Target{Key: "d", Root: true}
	cases := []struct {
		name string
		at   time.Time
		want map[string]string
	}{
		{"dernière sauvegarde", time.Time{}, map[string]string{"a.txt": "a2", "c.txt": "c2", "e.txt": "e1", "sub/f.txt": "f1"}},
		{"avant la première sauvegarde", day(-1), map[string]string{}},
		{"jour 1", day(1), map[string]string{"a.txt": "a1", "b.txt": "b1", "c.txt": "c1"}},
		{"jour 4 : c supprimé", day(4), map[string]string{"a.txt": "a1", "b.txt": "b1", "sub/f.txt": "f1"}},
		{"jour 9 : c recréé, b supprimé", day(9), map[string]string{"a.txt": "a1", "c.txt": "c2", "sub/f.txt": "f1"}},
		{"instant exact d'une version", day(10), map[string]string{"a.txt": "a2", "c.txt": "c2", "sub/f.txt": "f1"}},
	}
	for _, c := range cases {
		got := ids(StateAt(history(), root, c.at, false))
		if len(got) != len(c.want) {
			t.Errorf("%s : %v, attendu %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s : %s = %q, attendu %q", c.name, k, got[k], v)
			}
		}
	}

	files := StateAt(history(), root, day(1), false)
	byRel := map[string]File{}
	for _, f := range files {
		byRel[f.Rel] = f
	}
	if f := byRel["a.txt"]; !f.NewerSince || f.DeletedSince != nil {
		t.Errorf("a.txt : modifié depuis, pas supprimé : %+v", f)
	}
	if f := byRel["b.txt"]; f.DeletedSince == nil || !f.DeletedSince.Equal(day(5)) {
		t.Errorf("b.txt : supprimé le j5 : %+v", f)
	}
	if f := byRel["c.txt"]; f.DeletedSince != nil || !f.NewerSince {
		t.Errorf("c.txt : recréé depuis, donc pas supprimé : %+v", f)
	}
	if files[0].Rel != "a.txt" || files[len(files)-1].Rel != "c.txt" {
		t.Errorf("ordre : %v", files)
	}
	if n := Total(files); n != 60 {
		t.Errorf("total %d", n)
	}

	// cible fichier : Rel = nom du fichier
	f := StateAt(history(), Target{Key: "d/sub/f.txt"}, time.Time{}, false)
	if len(f) != 1 || f[0].Rel != "f.txt" {
		t.Fatalf("cible fichier : %+v", f)
	}
	// cible dossier
	f = StateAt(history(), Target{Key: "d/sub"}, time.Time{}, false)
	if len(f) != 1 || f[0].Rel != "f.txt" {
		t.Fatalf("cible dossier : %+v", f)
	}
	// casse ignorée (Windows)
	f = StateAt(history(), Target{Key: "D/SUB"}, time.Time{}, true)
	if len(f) != 1 || f[0].Rel != "f.txt" {
		t.Fatalf("casse ignorée : %+v", f)
	}
	if f := StateAt(history(), Target{Key: "D/SUB"}, time.Time{}, false); len(f) != 0 {
		t.Fatalf("casse respectée : %+v", f)
	}
}

func TestStateAtProblem(t *testing.T) {
	vs := []Version{
		{Key: "d/ok.txt", Latest: true, LastModified: day(0)},
		{Key: "d/../evasion.txt", Latest: true, LastModified: day(0)},
		{Key: "d/aux.txt", Latest: true, LastModified: day(0)},
	}
	problems := 0
	for _, f := range StateAt(vs, Target{Key: "d", Root: true}, time.Time{}, false) {
		if f.Problem != "" {
			problems++
		}
	}
	if problems != 2 {
		t.Fatalf("%d noms refusés, 2 attendus", problems)
	}
}

func TestHistory(t *testing.T) {
	h := History(history(), "d/c.txt", false)
	if len(h) != 3 || h[0].VersionID != "c2" || h[1].VersionID != "cx" || h[2].VersionID != "c1" {
		t.Fatalf("%+v", h)
	}
	if len(History(history(), "D/C.TXT", false)) != 0 || len(History(history(), "D/C.TXT", true)) != 3 {
		t.Fatal("casse")
	}
	if len(History(history(), "d/inconnu", false)) != 0 {
		t.Fatal("clé inconnue")
	}
}

func TestCheckRel(t *testing.T) {
	ok := []string{"a.txt", "Factures/2026/janvier.pdf", "été à Noël.docx", ".gitignore", "console/x", "COM10"}
	for _, r := range ok {
		if p := CheckRel(r); p != "" {
			t.Errorf("%q refusé : %s", r, p)
		}
	}
	bad := []string{"", "/etc/passwd", `a\b`, "a\x00b", "C:/x", "c:x", "../x", "a/../../x", "a/./b", "a//b",
		"a/", "x<y", "a:b", "q?.txt", "fin ", "fin.", "CON", "aux.txt", "Lpt1.log", "dir/NUL", "tab\tulation"}
	for _, r := range bad {
		if CheckRel(r) == "" {
			t.Errorf("%q accepté", r)
		}
	}
}
