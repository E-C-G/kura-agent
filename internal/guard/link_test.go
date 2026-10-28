package guard

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCheckSourceNotLink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "Documents")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckSourceNotLink(real); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "lien")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, real).CombinedOutput(); err != nil {
			t.Fatalf("mklink : %v %s", err, out)
		}
	} else if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	var le *LinkError
	if err := CheckSourceNotLink(link); !errors.As(err, &le) {
		t.Fatalf("lien accepté : %v", err)
	}
	if err := CheckSourceNotLink(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("source absente acceptée")
	}
}

func mkLink(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink : %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// Le parcours relève les liens et jonctions sans les suivre ; ceux des dossiers exclus
// sont ignorés.
func TestScanFindsLinks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	secret := filepath.Join(dir, "secret")
	for _, p := range []string{filepath.Join(src, "a", "f.txt"), filepath.Join(src, "tmp", "g.txt"), filepath.Join(secret, "x.secret")} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	mkLink(t, secret, filepath.Join(src, "a", "j"))
	mkLink(t, secret, filepath.Join(src, "tmp", "k"))
	c, err := Scan(src, []string{"tmp/*"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Files != 1 || len(c.Links) != 1 || c.Links[0] != "a/j" {
		t.Fatalf("%+v", c)
	}
	if rel, ok := LinkUnder(src, filepath.Join(src, "a", "j", "x.secret")); !ok || rel != "a/j" {
		t.Fatalf("LinkUnder : %q %v", rel, ok)
	}
	if _, ok := LinkUnder(src, filepath.Join(src, "a", "f.txt")); ok {
		t.Fatal("fichier ordinaire signalé")
	}
	if _, ok := LinkUnder(src, filepath.Join(dir, "ailleurs")); ok {
		t.Fatal("hors de la source")
	}
	if p := ExcludePattern("a/j [1]?*"); p[0] != "a/j [[]1][?][*]" || p[1] != "a/j [[]1][?][*]/*" {
		t.Fatalf("%q", p)
	}
}
