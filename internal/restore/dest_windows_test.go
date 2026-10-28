package restore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// Windows refuse même d'examiner un fichier ouvert sans partage : le classement le
	// signale, le remplacement aussi.
	if _, err := d.Classify(Item{File: File{Version: v}, Out: "boite.pst"}, true); err == nil || !strings.Contains(err.Error(), "ouvert par une application") {
		t.Fatalf("classement : %v", err)
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

// SDDL relu : « D:P » ou « D:PAI » selon l'héritage automatique.
var (
	privateACL    = regexp.MustCompile(`^D:P[A-Z]*\(A;;FA;;;SY\)\(A;;FA;;;BA\)$`)
	privateDirACL = regexp.MustCompile(`^D:P[A-Z]*\(A;OICI;FA;;;SY\)\(A;OICI;FA;;;BA\)$`)
)

func daclString(t *testing.T, p string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// Dossier à part : fichiers réservés aux administrateurs ; remplacement : ACL de l'actuel
// reprise, protection comprise.
func TestWindowsRights(t *testing.T) {
	d := newDest(t)
	d.Private = true
	m := &memBucket{data: map[string][]byte{"v1": []byte("bonjour")}}
	if err := d.Write(context.Background(), m, "a.txt", Version{Key: "k", VersionID: "v1", Size: 7}); err != nil {
		t.Fatal(err)
	}
	if s := daclString(t, filepath.Join(d.Dir, "a.txt")); !privateACL.MatchString(s) {
		t.Fatalf("fichier privé : %s", s)
	}

	d2 := newDest(t)
	cur := filepath.Join(d2.Dir, "b.txt")
	os.WriteFile(cur, []byte("actuel"), 0o644)
	sd, _ := windows.SecurityDescriptorFromString(privateFileSDDL)
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(cur, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d2.Replace(context.Background(), m, "b.txt", Version{Key: "k", VersionID: "v1", Size: 7}, "x"); err != nil {
		t.Fatal(err)
	}
	if s := daclString(t, cur); !privateACL.MatchString(s) {
		t.Fatalf("ACL non reprise : %s", s)
	}
}

// Dossier --to : créé privé ; un dossier existant appartenant à un autre compte est refusé.
func TestPrivateDirs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "R")
	if err := CreatePrivateDir(p); err != nil {
		t.Fatal(err)
	}
	if s := daclString(t, p); !privateDirACL.MatchString(s) {
		t.Fatalf("%s", s)
	}
	if err := SecureExistingDir(p); err != nil { // créé par le test (administrateur en CI)
		t.Logf("dossier existant : %v", err)
	}
	link := filepath.Join(t.TempDir(), "j")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, p).CombinedOutput(); err != nil {
		t.Fatalf("mklink : %v %s", err, out)
	}
	if err := SecureExistingDir(link); err == nil {
		t.Fatal("jonction acceptée")
	}
}
