package guard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/pkg/config"
)

func touch(t *testing.T, root string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		p := filepath.Join(root, filepath.FromSlash(r))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCountFilesAppliesExcludes(t *testing.T) {
	root := t.TempDir()
	touch(t, root,
		"Compta/bilan.xlsx",
		"Compta/2026/janvier.pdf",
		"$RECYCLE.BIN/S-1-5-21/old.doc",
		"System Volume Information/tracking.log",
		"Clients/devis.docx",
	)
	c, err := CountFiles(root, []string{"*$RECYCLE.BIN*", "*System Volume Information*"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Files != 3 {
		t.Errorf("3 fichiers attendus hors exclusions, reçu %d", c.Files)
	}
	// Motifs du script d'origine, avec séparateurs Windows.
	c, err = CountFiles(root, []string{`*\$RECYCLE.BIN\*`, `*\System Volume Information\*`})
	if err != nil {
		t.Fatal(err)
	}
	if c.Files != 3 {
		t.Errorf("motifs à la Windows : 3 fichiers attendus, reçu %d", c.Files)
	}
}

func TestCountFilesMissingSource(t *testing.T) {
	if _, err := CountFiles(filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Fatal("une source absente doit être une erreur, jamais « 0 fichier »")
	}
}

func TestCountFilesSkipsUnreadableDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod ne retire pas le droit de lecture sous Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root lit tout")
	}
	root := t.TempDir()
	touch(t, root, "ok/a.txt", "secret/b.txt")
	os.Chmod(filepath.Join(root, "secret"), 0)
	defer os.Chmod(filepath.Join(root, "secret"), 0o755)
	c, err := CountFiles(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Files != 1 || c.Unreadable != 1 {
		t.Errorf("attendu 1 fichier et 1 illisible, reçu %+v", c)
	}
}

func TestEvaluate(t *testing.T) {
	g := config.Guard{MinSourceFiles: 1000, MaxDeletes: 500, MaxDeleteRatio: 0.05}
	cases := []struct {
		name           string
		files, deletes int
		blocked        bool
		reason         string
	}{
		{"nuit normale", 20000, 12, false, ""},
		{"source démontée", 0, 20000, true, "source vide ou démontée"},
		{"trop de suppressions", 20000, 600, true, "plus que le maximum de 500"},
		{"ratio dépassé", 2000, 200, true, "%"},
		{"pile au seuil", 1000, 0, false, ""},
	}
	for _, tc := range cases {
		d := Evaluate(g, tc.files, tc.deletes)
		if d.Blocked != tc.blocked || !strings.Contains(d.Reason, tc.reason) {
			t.Errorf("%s : %+v", tc.name, d)
		}
	}
	// Plafonds à 0 = désactivés
	if d := Evaluate(config.Guard{MinSourceFiles: 1}, 10, 10000); d.Blocked {
		t.Errorf("sans plafond, seul le minimum de fichiers compte : %+v", d)
	}
}
