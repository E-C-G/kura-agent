package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/pkg/config"
	"github.com/E-C-G/kura-agent/pkg/pack"
)

type importHarness struct {
	im      importer
	out     *bytes.Buffer
	file    string
	pw      string
	tested  []creds.AWSKeys
	testErr error
}

func newImportHarness(t *testing.T, mut func(*pack.Contents)) *importHarness {
	t.Helper()
	dir := t.TempDir()
	c := pack.Contents{Kind: pack.KindCredentials, CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(72 * time.Hour),
		Client: "acme", Host: "SRV01", Bucket: "acme-srv01",
		Credentials: []pack.Credential{{Name: "wasabi", Kind: "aws", AccessKeyID: "AKIDNOUVELLE1", SecretAccessKey: "secret-neuf"}}}
	if mut != nil {
		mut(&c)
	}
	pw, _ := pack.NewPassword()
	data, err := pack.Seal(c, pw, 1000)
	if err != nil {
		t.Fatal(err)
	}
	h := &importHarness{out: &bytes.Buffer{}, file: filepath.Join(dir, "poste.kura"), pw: pw}
	os.WriteFile(h.file, data, 0o600)
	cfg := &config.Config{}
	cfg.Profile.Bucket = "acme-srv01"
	cfg.Profile.AWSCredentials = "credential:wasabi"
	h.im = importer{
		out: h.out, now: time.Now, checkAdmin: func() error { return nil },
		ask:        func(string) (string, error) { return h.pw, nil },
		store:      creds.Store{Dir: filepath.Join(dir, "credentials")},
		loadConfig: func(string) (*config.Config, error) { return cfg, nil },
		testKey: func(_ *config.Config, k creds.AWSKeys) error {
			h.tested = append(h.tested, k)
			return h.testErr
		},
		remove: os.Remove,
	}
	return h
}

func TestImportStoresTestedKey(t *testing.T) {
	h := newImportHarness(t, nil)
	h.im.store.Init()
	h.im.store.Set("wasabi", "ANCIENNE:ancien")
	if err := h.im.run(h.file, "config.yaml", false, false, false); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if len(h.tested) != 1 || h.tested[0].AccessKeyID != "AKIDNOUVELLE1" {
		t.Fatalf("clé non testée : %+v", h.tested)
	}
	v, _ := h.im.store.Get("wasabi")
	if k, _ := creds.ParseAWSKeys(v); k.AccessKeyID != "AKIDNOUVELLE1" {
		t.Fatalf("coffre : %q", v)
	}
	if _, err := os.Stat(h.file); !os.IsNotExist(err) {
		t.Fatal("paquet non supprimé après import")
	}
	if strings.Contains(h.out.String(), "secret-neuf") {
		t.Fatal("secret affiché")
	}
}

func TestImportRefusedKeyLeavesOldOne(t *testing.T) {
	h := newImportHarness(t, nil)
	h.im.store.Init()
	h.im.store.Set("wasabi", "ANCIENNE:ancien")
	h.testErr = errors.New("403 Forbidden")
	if err := h.im.run(h.file, "config.yaml", false, false, false); err == nil || !strings.Contains(err.Error(), "rien n'a été modifié") {
		t.Fatalf("clé refusée importée : %v", err)
	}
	if v, _ := h.im.store.Get("wasabi"); v != "ANCIENNE:ancien" {
		t.Fatalf("ancienne clé remplacée : %q", v)
	}
	if _, err := os.Stat(h.file); err != nil {
		t.Fatal("paquet supprimé malgré l'échec")
	}
}

func TestImportChecks(t *testing.T) {
	h := newImportHarness(t, func(c *pack.Contents) { c.Bucket = "autre-bucket" })
	if err := h.im.run(h.file, "config.yaml", false, false, true); err == nil || !strings.Contains(err.Error(), "autre-bucket") {
		t.Fatalf("bucket différent accepté : %v", err)
	}
	h = newImportHarness(t, func(c *pack.Contents) { c.ExpiresAt = time.Now().Add(-time.Minute) })
	if err := h.im.run(h.file, "config.yaml", false, false, true); err == nil || !strings.Contains(err.Error(), "expiré") {
		t.Fatalf("paquet expiré accepté : %v", err)
	}
	h = newImportHarness(t, nil)
	tries := 0
	h.im.ask = func(string) (string, error) { tries++; return "MAUVAIS-MOT-DE-PASSE", nil }
	if err := h.im.run(h.file, "config.yaml", false, false, true); !errors.Is(err, pack.ErrWrongPassword) || tries != 3 {
		t.Fatalf("mot de passe : %v après %d essais", err, tries)
	}
	h = newImportHarness(t, nil)
	h.im.checkAdmin = func() error { return errors.New("droits d'administrateur requis") }
	if err := h.im.run(h.file, "config.yaml", false, false, true); err == nil {
		t.Fatal("import sans droits d'administrateur")
	}
}

// Un nom de secret différent de celui de la configuration : refusé sans --force, la
// clé ne servirait pas et la confirmation dans la console couperait la sauvegarde.
func TestImportNameMismatch(t *testing.T) {
	h := newImportHarness(t, func(c *pack.Contents) { c.Credentials[0].Name = "wasabi-acme" })
	h.im.store.Init()
	h.im.store.Set("wasabi", "ANCIENNE:ancien")
	if err := h.im.run(h.file, "config.yaml", false, false, false); err == nil || !strings.Contains(err.Error(), "ne servirait pas") {
		t.Fatalf("nom différent accepté : %v", err)
	}
	if _, err := h.im.store.Get("wasabi-acme"); err == nil {
		t.Fatal("secret enregistré malgré le refus")
	}
}

func TestSealRefusesNameTheVaultRejects(t *testing.T) {
	for _, name := range []string{"Wasabi.Acme", "wasabi_acme", "", strings.Repeat("a", 41)} {
		c := pack.Contents{Kind: pack.KindCredentials, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Bucket: "b",
			Credentials: []pack.Credential{{Name: name, Kind: "aws", AccessKeyID: "A", SecretAccessKey: "S"}}}
		if _, err := pack.Seal(c, "MOT-DE-PASSE", 1000); err == nil {
			t.Errorf("nom %q accepté par le paquet", name)
		}
	}
}
