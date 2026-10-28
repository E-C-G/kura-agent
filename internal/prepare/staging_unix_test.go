//go:build !windows

package prepare

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureStaging(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	if err := EnsureStaging(staging); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(staging); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("zone : %v %v", fi.Mode(), err)
	}
	os.Chmod(staging, 0o755)
	if err := EnsureStaging(staging); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(staging); fi.Mode().Perm() != 0o700 {
		t.Fatalf("droits non corrigés : %v", fi.Mode())
	}
	// Un lien posé à la place de la zone est refusé, jamais suivi.
	other := filepath.Join(dir, "ailleurs")
	os.Mkdir(other, 0o755)
	link := filepath.Join(dir, "lien")
	os.Symlink(other, link)
	if err := EnsureStaging(link); err == nil {
		t.Fatal("lien accepté comme zone de préparation")
	}
	if fi, _ := os.Stat(other); fi.Mode().Perm() != 0o755 {
		t.Fatal("la cible du lien a été modifiée")
	}
}
