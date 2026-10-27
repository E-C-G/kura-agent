package creds

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCreds imite systemd-creds : le nom est inscrit dans le chiffré et vérifié.
func fakeCreds(stdin []byte, args ...string) ([]byte, error) {
	name := strings.TrimPrefix(args[1], "--name=")
	switch args[0] {
	case "encrypt":
		return append([]byte("ENC["+name+"]"), bytes.ToUpper(stdin)...), nil
	case "decrypt":
		head := []byte("ENC[" + name + "]")
		if !bytes.HasPrefix(stdin, head) {
			return nil, errors.New("nom différent")
		}
		return bytes.ToLower(stdin[len(head):]), nil
	}
	return nil, errors.New("commande inconnue")
}

func linuxStore(dir, service string) Store {
	return Store{Dir: dir, cipher: systemdCreds{run: fakeCreds}, prefix: Prefix, serviceDir: service}
}

func TestSystemdCredsStore(t *testing.T) {
	dir := t.TempDir()
	s := linuxStore(dir, "")
	if err := s.Set("wasabi", "akid:secret"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "kura.wasabi"))
	if err != nil || !bytes.HasPrefix(raw, []byte("ENC[kura.wasabi]")) || bytes.Contains(raw, []byte("akid:secret")) {
		t.Fatalf("fichier : %q %v", raw, err)
	}
	if v, err := s.Get("wasabi"); err != nil || v != "akid:secret" {
		t.Fatalf("%q %v", v, err)
	}
	// Fichier renommé : refusé (nom inscrit dans le chiffré).
	os.Rename(filepath.Join(dir, "kura.wasabi"), filepath.Join(dir, "kura.autre"))
	if _, err := s.Get("autre"); err == nil {
		t.Fatal("secret renommé accepté")
	}
	// Autres secrets de la machine (sans préfixe kura.) : ignorés.
	os.WriteFile(filepath.Join(dir, "autre-service.cred"), []byte("x"), 0o600)
	s.Set("hc-url", "https://hc")
	if names, err := s.List(); err != nil || strings.Join(names, ",") != "autre,hc-url" {
		t.Fatalf("liste : %v %v", names, err)
	}
	if _, err := s.Get("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent : %v", err)
	}
}

// Sous le service, systemd a déjà déchiffré le secret dans $CREDENTIALS_DIRECTORY : il
// est lu là, sans toucher au fichier chiffré (illisible pour le compte kura-agent).
func TestServiceCredentialsDirectory(t *testing.T) {
	service := t.TempDir()
	os.WriteFile(filepath.Join(service, "kura.wasabi"), []byte("akid:service"), 0o400)
	s := linuxStore(filepath.Join(t.TempDir(), "illisible"), service)
	if v, err := s.Get("wasabi"); err != nil || v != "akid:service" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := s.Get("hc-url"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent du service et du coffre : %v", err)
	}
}

// Le vrai systemd-creds, si on peut s'en servir (root, Linux).
func TestRealSystemdCreds(t *testing.T) {
	if _, err := exec.LookPath("systemd-creds"); err != nil || os.Geteuid() != 0 {
		t.Skip("systemd-creds absent ou pas root")
	}
	dir := t.TempDir()
	s := Store{Dir: dir, cipher: systemdCreds{run: runSystemdCreds}, prefix: Prefix}
	if err := s.Set("essai", "akid:secret"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "kura.essai")); bytes.Contains(raw, []byte("akid:secret")) {
		t.Fatal("secret en clair")
	}
	if v, err := s.Get("essai"); err != nil || v != "akid:secret" {
		t.Fatalf("%q %v", v, err)
	}
}
