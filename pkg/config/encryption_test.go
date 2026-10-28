package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"filippo.io/age"
	"golang.org/x/crypto/ssh"
)

const fakeGPGKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEZx+…
-----END PGP PUBLIC KEY BLOCK-----`

func ageKey(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

func sshKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func withEncryption(t *testing.T) string {
	t.Helper()
	return `
version: 1
profile:
  endpoint: https://s3.eu-central-1.wasabisys.com
  bucket: acme-srv01
schedule:
  type: daily
  time: "22:30"
tools:
  pg_dump: /usr/lib/postgresql/16/bin/pg_dump
  zip: /usr/bin/zip
  gpg: /usr/bin/gpg
encryption:
  gpg:
    recipients:
      - fingerprint: "0123 4567 89ab cdef 0123  4567 89AB CDEF 0123 4567"
        key: |
` + indent(fakeGPGKey, "          ") + `
  age:
    recipients:
      - ` + ageKey(t) + `
prepare:
  - id: compta-db
    type: db_export
    engine: postgresql
    user: kura_backup
    password: credential:compta-db
  - id: compta-chiffre
    type: age
    input: compta-db
  - id: courrier
    type: zip
    source: /srv/courrier
  - id: courrier-chiffre
    type: gpg
    input: courrier
  - id: contrats
    type: age
    source: /srv/contrats
    recipients:
      - "` + sshKey(t) + `"
jobs:
  - id: compta
    source: "${STAGING:compta-chiffre}"
    destination: BASES/COMPTA
  - id: courrier
    source: "${STAGING:courrier-chiffre}"
    destination: COURRIER
  - id: contrats
    source: "${STAGING:contrats}"
    destination: CONTRATS
`
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

func TestEncryptionLoads(t *testing.T) {
	c, err := Parse([]byte(withEncryption(t)), LoadOptions{Env: testEnv()})
	if err != nil {
		t.Fatal(err)
	}
	gpg, _ := c.PrepareByID("courrier-chiffre")
	rs := c.RecipientsOf(gpg)
	if len(rs) != 1 || NormalizeFingerprint(rs[0].Fingerprint) != "0123456789ABCDEF0123456789ABCDEF01234567" || !strings.Contains(rs[0].Key, "BEGIN PGP PUBLIC") {
		t.Fatalf("destinataires gpg : %+v", rs)
	}
	ag, _ := c.PrepareByID("contrats")
	if rs := c.RecipientsOf(ag); len(rs) != 1 || !strings.HasPrefix(rs[0].Age, "ssh-ed25519 ") {
		t.Fatalf("destinataires age propres : %+v", rs)
	}
	if e, ok := c.EncrypterOf("compta-db"); !ok || e.ID != "compta-chiffre" {
		t.Fatalf("chiffrement de compta-db : %+v", e)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Fatalf("avertissements : %v", w)
	}
}

func TestEncryptionValidation(t *testing.T) {
	base := withEncryption(t)
	cases := []struct {
		name, from, to, field string
	}{
		{"clair envoyé", "${STAGING:courrier-chiffre}", "${STAGING:courrier}", "jobs[1].source"},
		{"entrée inconnue", "input: courrier\n", "input: autre\n", "prepare[3].input"},
		{"entrée après", "  - id: compta-chiffre\n    type: age\n    input: compta-db\n", "  - id: compta-chiffre\n    type: age\n    input: courrier-chiffre\n", "prepare[1].input"},
		{"double chiffrement", "input: courrier\n", "input: compta-chiffre\n", "prepare[3].input"},
		{"entrée chiffrée deux fois", "input: courrier\n", "input: compta-db\n", "prepare[3].input"},
		{"input et source", "input: courrier\n", "input: courrier\n    source: /srv/x\n", "prepare[3]"},
		{"ni input ni source", "    source: /srv/contrats\n", "", "prepare[4]"},
		{"source relative", "source: /srv/contrats", "source: contrats", "prepare[4].source"},
		{"tools.gpg absent", "  gpg: /usr/bin/gpg\n", "", "prepare[3].type"},
		{"empreinte courte", `"0123 4567 89ab cdef 0123  4567 89AB CDEF 0123 4567"`, `"0123456789ABCDEF"`, "encryption.gpg.recipients[0]"},
		{"clé privée gpg", "BEGIN PGP PUBLIC KEY BLOCK", "BEGIN PGP PRIVATE KEY BLOCK", "encryption.gpg.recipients[0]"},
		{"identité age privée", "      - age1", "      - AGE-SECRET-KEY-1QQQ\n      - age1", "encryption.age.recipients[0]"},
		{"clé age illisible", "      - age1", "      - age1nimportequoi\n      - age1", "encryption.age.recipients[0]"},
		{"clé gpg pour age", "      - \"ssh-", "      - fingerprint: 0123456789ABCDEF0123456789ABCDEF01234567\n        key: x\n      - \"ssh-", "prepare[4].recipients[0]"},
		{"champ inconnu", "      - fingerprint: \"0123", "      - empreinte: x\n        fingerprint: \"0123", "empreinte"},
		{"clés réservées", "input: courrier\n", "input: courrier\n    exclude: [\"*.tmp\"]\n", "prepare[3]"},
		{"input pour zip", "source: /srv/courrier", "source: /srv/courrier\n    input: compta-db", "prepare[2]"},
	}
	for _, c := range cases {
		yaml := strings.Replace(base, c.from, c.to, 1)
		if yaml == base {
			t.Fatalf("%s : %q introuvable", c.name, c.from)
		}
		err := parseWith(t, yaml, LoadOptions{})
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s : erreur sur %s attendue, reçu %v", c.name, c.field, err)
		}
	}
	// Sans destinataires nulle part : ni dans la préparation, ni dans encryption.age.
	i, j := strings.Index(base, "  age:\n"), strings.Index(base, "prepare:\n")
	yaml := base[:i] + base[j:]
	if err := parseWith(t, yaml, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "prepare[1].recipients") {
		t.Errorf("age sans destinataires : %v", err)
	}
}

func TestAgeRecipients(t *testing.T) {
	if _, h, err := ParseAgeRecipient(ageKey(t)); err != nil || h {
		t.Fatalf("clé age : %v %v", h, err)
	}
	if _, _, err := ParseAgeRecipient(sshKey(t)); err != nil {
		t.Fatalf("clé SSH : %v", err)
	}
	id, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pq := id.Recipient().String()
	if _, h, err := ParseAgeRecipient(pq); err != nil || !h {
		t.Fatalf("clé hybride : %v %v", h, err)
	}
	if _, _, err := ParseAgeRecipient("AGE-SECRET-KEY-1ABCDEF"); err == nil || strings.Contains(err.Error(), "ABCDEF") {
		t.Fatalf("identité privée : %v", err)
	}
	var errs []string
	validateRecipients(PrepareAge, "r", []Recipient{{Age: ageKey(t)}, {Age: pq}}, func(f, format string, a ...any) {
		errs = append(errs, f)
	})
	if len(errs) != 1 || errs[0] != "r" {
		t.Fatalf("clés hybrides et classiques mêlées : %v", errs)
	}
}

func TestEncryptionWarnings(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(withEncryption(t), "    input: courrier\n", "    input: courrier\n    recipients:\n      - fingerprint: 89ABCDEF0123456789ABCDEF0123456789ABCDEF\n        key: |\n"+indent(fakeGPGKey, "          ")+"\n", 1)),
		LoadOptions{Env: testEnv()})
	if err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "encryption.gpg") {
		t.Fatalf("%v", w)
	}
}
