package restore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
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

// Un fichier ouvert sans partage (Outlook et son .pst) n'est pas remplacé : rien ne
// change, l'erreur le signale.
func TestReplaceLockedFile(t *testing.T) {
	d := newDest(t)
	p := filepath.Join(d.Dir, "boite.pst")
	os.WriteFile(p, []byte("actuel"), 0o644)
	name, _ := windows.UTF16PtrFromString(p)
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := &memBucket{data: map[string][]byte{"v1": []byte("ancienne")}}
	v := Version{Key: "k", VersionID: "v1", Size: 8, ETag: `"00000000000000000000000000000000"`}
	if _, err := d.Classify(Item{File: File{Version: v}, Out: "boite.pst"}, true); err != nil {
		t.Logf("classement : %v", err) // tailles différentes : pas de lecture nécessaire
	}
	_, err = d.Replace(context.Background(), m, "boite.pst", v, "x")
	if err == nil || !strings.Contains(err.Error(), "ouvert par une application") {
		t.Fatalf("fichier verrouillé remplacé : %v", err)
	}
	windows.CloseHandle(h)
	if got, _ := os.ReadFile(p); string(got) != "actuel" {
		t.Fatalf("fichier touché : %q", got)
	}
	entries, _ := os.ReadDir(d.Dir)
	if len(entries) != 1 {
		t.Fatalf("restes : %v", entries)
	}
}
