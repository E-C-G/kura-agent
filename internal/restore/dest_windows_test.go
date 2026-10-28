package restore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Une jonction (mklink /J), que tout utilisateur peut créer sans droits particuliers,
// est refusée comme un lien symbolique.
func TestDestRefusesJunction(t *testing.T) {
	outside := t.TempDir()
	d := newDest(t)
	link := filepath.Join(d.Dir, "Documents")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink : %v %s", err, out)
	}
	m := &memBucket{data: map[string][]byte{"v1": []byte("bonjour")}}
	if err := d.Write(context.Background(), m, "Documents/a.txt", Version{Key: "k", VersionID: "v1", Size: 7}); !errors.Is(err, ErrLink) {
		t.Fatalf("jonction suivie : %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("écrit hors de la destination : %v", entries)
	}
	if _, err := OpenDest(link, Owner{}); err == nil {
		t.Fatal("destination jonction acceptée")
	}
}
