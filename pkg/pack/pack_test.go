package pack

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func sample() Contents {
	return Contents{Kind: KindCredentials, CreatedAt: time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC), Client: "acme", Host: "SRV01", Bucket: "acme-srv01",
		Credentials: []Credential{{Name: "wasabi", Kind: "aws", AccessKeyID: "AKIDPOSTE", SecretAccessKey: "secretposte"}}}
}

func TestRoundTrip(t *testing.T) {
	pw, err := NewPassword()
	if err != nil || len(pw) != 24 || strings.Count(pw, "-") != 4 {
		t.Fatalf("mot de passe %q %v", pw, err)
	}
	data, err := Seal(sample(), pw, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("secretposte")) || bytes.Contains(data, []byte("AKIDPOSTE")) {
		t.Fatal("secret en clair dans le paquet")
	}
	// Retapé en minuscules, sans tirets : accepté.
	c, err := Open(data, strings.ToLower(strings.ReplaceAll(pw, "-", "")))
	if err != nil || c.Credentials[0].SecretAccessKey != "secretposte" || c.Bucket != "acme-srv01" {
		t.Fatalf("ouverture : %+v %v", c, err)
	}
	if !c.Expired(c.ExpiresAt.Add(time.Second)) || c.Expired(c.CreatedAt) {
		t.Fatal("Expired")
	}
	if _, err := Open(data, "AAAA-BBBB-CCCC-DDDD-EEEE"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("mauvais mot de passe : %v", err)
	}
}

func TestTamperedHeaderRejected(t *testing.T) {
	pw, _ := NewPassword()
	data, _ := Seal(sample(), pw, 1000)
	var env map[string]any
	json.Unmarshal(data, &env)
	env["iter"] = 1001 // en-tête authentifié : toute modification est détectée
	bad, _ := json.Marshal(env)
	if _, err := Open(bad, pw); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("en-tête modifié accepté : %v", err)
	}
	env["iter"] = 50_000_000
	bad, _ = json.Marshal(env)
	if _, err := Open(bad, pw); !errors.Is(err, ErrFormat) {
		t.Fatalf("itérations démesurées : %v", err)
	}
	if _, err := Open([]byte("PK\x03\x04 zip"), pw); !errors.Is(err, ErrFormat) {
		t.Fatalf("autre fichier : %v", err)
	}
}

func TestValidate(t *testing.T) {
	for name, mut := range map[string]func(*Contents){
		"type inconnu":     func(c *Contents) { c.Kind = "full" },
		"sans bucket":      func(c *Contents) { c.Bucket = "" },
		"sans clé":         func(c *Contents) { c.Credentials = nil },
		"nom invalide":     func(c *Contents) { c.Credentials[0].Name = `..\x` },
		"paire incomplète": func(c *Contents) { c.Credentials[0].SecretAccessKey = "" },
	} {
		c := sample()
		mut(&c)
		if _, err := Seal(c, "AAAA-BBBB-CCCC-DDDD-EEEE", 1000); err == nil {
			t.Errorf("%s : accepté", name)
		}
	}
	if _, err := Seal(sample(), "court", 1000); err == nil {
		t.Error("mot de passe court accepté")
	}
}

func TestPasswordAlphabetUnambiguous(t *testing.T) {
	for i := 0; i < 200; i++ {
		pw, _ := NewPassword()
		if strings.ContainsAny(pw, "01OIL") {
			t.Fatalf("caractère ambigu dans %q", pw)
		}
	}
}
