package creds

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// fakeSecurity imite /usr/bin/security pour un trousseau en mémoire.
type fakeSecurity struct {
	items map[string]string // service/compte -> mot de passe
	calls []string
	stdin []string
	// broken : add-generic-password accepté sans rien ranger (security -i)
	broken bool
}

var addRe = regexp.MustCompile(`^add-generic-password -s (\S+) -a (\S+) -l Kura -A -w (\S+) (\S+)\n$`)

func (f *fakeSecurity) run(stdin string, args ...string) (string, int, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	f.stdin = append(f.stdin, stdin)
	switch args[0] {
	case "find-generic-password":
		v, ok := f.items[args[2]+"/"+args[4]]
		if !ok {
			return "", notFoundCode, errors.New("The specified item could not be found in the keychain.")
		}
		return v + "\n", 0, nil
	case "-i":
		m := addRe.FindStringSubmatch(stdin)
		if m == nil {
			return "", 1, errors.New("commande inattendue : " + stdin)
		}
		if !f.broken {
			f.items[m[1]+"/"+m[2]] = m[3]
		}
		return "", 0, nil
	}
	return "", 1, errors.New("inattendu")
}

func TestKeychainVault(t *testing.T) {
	f := &fakeSecurity{items: map[string]string{}}
	s := Store{Dir: t.TempDir(), cipher: newKeychainCipher(SystemKeychain, f.run)}
	// Rien dans le trousseau : un Get ne crée pas de clé.
	if _, err := s.Get("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Set("hc-url", "https://hc-ping.example/uuid"); err != nil {
		t.Fatal(err)
	}
	if len(f.items) != 1 {
		t.Fatalf("clé non créée : %v", f.calls)
	}
	// La clé ne passe jamais en argument : seulement sur l'entrée standard.
	for _, c := range f.calls {
		for _, v := range f.items {
			if strings.Contains(c, v) {
				t.Fatalf("clé en argument : %s", c)
			}
		}
	}
	raw, _ := os.ReadFile(filepath.Join(s.Dir, "hc-url"+ext))
	if strings.Contains(string(raw), "hc-ping") || !strings.HasPrefix(string(raw), keychainMagic) {
		t.Fatalf("secret en clair : %q", raw)
	}
	// Nouveau processus (clé relue dans le trousseau).
	s2 := Store{Dir: s.Dir, cipher: newKeychainCipher(SystemKeychain, f.run)}
	if v, err := s2.Get("hc-url"); err != nil || v != "https://hc-ping.example/uuid" {
		t.Fatalf("%q %v", v, err)
	}
	// Fichier renommé : refusé (nom authentifié).
	if err := os.Rename(filepath.Join(s.Dir, "hc-url"+ext), filepath.Join(s.Dir, "autre"+ext)); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get("autre"); err == nil {
		t.Fatal("secret renommé accepté")
	}
	// Clé disparue du trousseau : refus clair, pas de nouvelle clé au déchiffrement.
	s3 := Store{Dir: s.Dir, cipher: newKeychainCipher(SystemKeychain, (&fakeSecurity{items: map[string]string{}}).run)}
	if _, err := s3.Get("autre"); err == nil || !strings.Contains(err.Error(), "absente du trousseau") {
		t.Fatal(err)
	}
}

func TestKeychainCreationNotStored(t *testing.T) {
	f := &fakeSecurity{items: map[string]string{}, broken: true}
	s := Store{Dir: t.TempDir(), cipher: newKeychainCipher(SystemKeychain, f.run)}
	if err := s.Set("x", "valeur"); err == nil || !strings.Contains(err.Error(), "introuvable après création") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "x"+ext)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("secret écrit sans clé")
	}
}

func TestKeychainPathRefused(t *testing.T) {
	f := &fakeSecurity{items: map[string]string{}}
	s := Store{Dir: t.TempDir(), cipher: newKeychainCipher("/tmp/a b.keychain", f.run)}
	if err := s.Set("x", "v"); err == nil {
		t.Fatal("chemin avec espace accepté")
	}
}

// Avec le vrai /usr/bin/security, sur un trousseau jetable (CI macOS seulement : la
// création ajoute le trousseau à la liste de recherche de l'utilisateur).
func TestKeychainWithSecurity(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("CI") == "" {
		t.Skip("CI macOS seulement")
	}
	dir, err := os.MkdirTemp("/tmp", "kura-kc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	kc := filepath.Join(dir, "test.keychain")
	if out, err := exec.Command("security", "create-keychain", "-p", "essai", kc).CombinedOutput(); err != nil {
		t.Fatalf("create-keychain : %v %s", err, out)
	}
	defer exec.Command("security", "delete-keychain", kc).Run()
	s := Store{Dir: filepath.Join(dir, "credentials"), cipher: newKeychainCipher(kc, runSecurity)}
	if err := s.Set("wasabi", "AKID:secret"); err != nil {
		t.Fatal(err)
	}
	s2 := Store{Dir: s.Dir, cipher: newKeychainCipher(kc, runSecurity)}
	if v, err := s2.Get("wasabi"); err != nil || v != "AKID:secret" {
		t.Fatalf("%q %v", v, err)
	}
}
