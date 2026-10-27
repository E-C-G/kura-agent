package lock

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSecondAcquireIsBusy(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireIn(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("ErrBusy attendu, reçu %v", err)
	}
	release()
	again, err := AcquireIn(dir)
	if err != nil {
		t.Fatalf("après libération, le verrou doit se reprendre : %v", err)
	}
	again()
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(dir, FileName)); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("droits du verrou : %v %v", st, err)
		}
	}
}

func TestMissingDir(t *testing.T) {
	if _, err := AcquireIn(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("verrou sans dossier de données")
	}
}

func TestLockIsNotASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("liens symboliques : droits particuliers sous Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "ailleurs")
	if err := os.Symlink(target, filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireIn(dir); err == nil {
		t.Fatal("verrou suivi à travers un lien symbolique")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fichier créé au bout du lien")
	}
}
