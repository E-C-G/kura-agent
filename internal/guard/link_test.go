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
