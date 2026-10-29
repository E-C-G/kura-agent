package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"go.yaml.in/yaml/v3"
)

// Encryption donne les destinataires par défaut des préparations gpg et age : une
// préparation sans recipients prend ceux-ci ; avec recipients, seulement les siens.
type Encryption struct {
	GPG EncryptionKeys `yaml:"gpg"`
	Age EncryptionKeys `yaml:"age"`
}

// EncryptionKeys : destinataires d'un format de chiffrement.
type EncryptionKeys struct {
	Recipients []Recipient `yaml:"recipients"`
}

// Recipient : destinataire d'un chiffrement, clé publique seulement (la clé privée ne
// vient jamais sur le poste). gpg : empreinte complète et clé publique ASCII
// (fingerprint, key) ; age : une ligne de clé publique (age1…, age1pq1…, ssh-ed25519 …,
// ssh-rsa …), écrite seule dans la liste.
type Recipient struct {
	Fingerprint string `yaml:"fingerprint"`
	Key         string `yaml:"key"`
	Age         string `yaml:"-"`
}

// UnmarshalYAML : une ligne seule est un destinataire age ; une table, une clé gpg
// (fingerprint et key, rien d'autre).
func (r *Recipient) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&r.Age)
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			var dst *string
			switch k.Value {
			case "fingerprint":
				dst = &r.Fingerprint
			case "key":
				dst = &r.Key
			default:
				return fmt.Errorf("ligne %d : champ %q inconnu dans un destinataire (fingerprint, key)", k.Line, k.Value)
			}
			if err := v.Decode(dst); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("ligne %d : destinataire attendu : clé age sur une ligne, ou fingerprint et key (gpg)", n.Line)
}

// MarshalYAML : forme inverse d'UnmarshalYAML (utile à kura-ctl).
func (r Recipient) MarshalYAML() (any, error) {
	if r.Age != "" {
		return r.Age, nil
	}
	return struct {
		Fingerprint string `yaml:"fingerprint"`
		Key         string `yaml:"key"`
	}{r.Fingerprint, r.Key}, nil
}

// RecipientsOf : destinataires d'une préparation gpg ou age, les siens ou ceux de la
// section encryption.
func (c *Config) RecipientsOf(p Prepare) []Recipient {
	if len(p.Recipients) > 0 {
		return p.Recipients
	}
	switch p.Type {
	case PrepareGPG:
		return c.Encryption.GPG.Recipients
	case PrepareAge:
		return c.Encryption.Age.Recipients
	}
	return nil
}

// EncrypterOf : préparation gpg ou age qui chiffre le résultat de la préparation id.
func (c *Config) EncrypterOf(id string) (Prepare, bool) {
	for _, p := range c.Prepare {
		if p.Input != "" && p.Input == id {
			return p, true
		}
	}
	return Prepare{}, false
}

// Warnings : ce qui n'empêche pas de sauvegarder mais mérite d'être lu (validate,
// install) : clés de la section encryption qu'aucune préparation n'utilise.
func (c *Config) Warnings() []string {
	var out []string
	for _, t := range []struct {
		typ, field string
		keys       []Recipient
	}{{PrepareGPG, "encryption.gpg", c.Encryption.GPG.Recipients}, {PrepareAge, "encryption.age", c.Encryption.Age.Recipients}} {
		if len(t.keys) == 0 {
			continue
		}
		used := false
		for _, p := range c.Prepare {
			used = used || p.Type == t.typ && len(p.Recipients) == 0
		}
		if !used {
			out = append(out, fmt.Sprintf("%s : destinataires déclarés mais utilisés par aucune préparation %s", t.field, t.typ))
		}
	}
	return out
}

var fingerprintPattern = regexp.MustCompile(`^[0-9A-F]{40}$`)

// NormalizeFingerprint : empreinte OpenPGP en majuscules, sans espaces.
func NormalizeFingerprint(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// checkGPGRecipient vérifie la forme d'une clé gpg ; la clé elle-même (empreinte,
// expiration, révocation) est vérifiée par gpg sur le poste.
func checkGPGRecipient(r Recipient) error {
	switch {
	case r.Age != "":
		return errors.New("clé gpg attendue : fingerprint (empreinte complète) et key (clé publique ASCII)")
	case !fingerprintPattern.MatchString(NormalizeFingerprint(r.Fingerprint)):
		return fmt.Errorf("fingerprint %q : empreinte complète attendue, 40 caractères hexadécimaux", r.Fingerprint)
	case strings.Contains(r.Key, "PRIVATE KEY"):
		return errors.New("key : clé privée refusée ; seule la clé publique vient sur le poste")
	case !strings.Contains(r.Key, "-----BEGIN PGP PUBLIC KEY BLOCK-----") || !strings.Contains(r.Key, "-----END PGP PUBLIC KEY BLOCK-----"):
		return errors.New("key : clé publique ASCII attendue (-----BEGIN PGP PUBLIC KEY BLOCK----- … -----END PGP PUBLIC KEY BLOCK-----)")
	}
	return nil
}

// ParseAgeRecipient lit un destinataire age : clé age (age1…), hybride post-quantique
// (age1pq1…) ou clé publique SSH (ssh-ed25519, ssh-rsa). hybrid dit si la clé est
// hybride : age refuse de mêler des clés hybrides et classiques dans un même fichier.
// Une identité privée est refusée sans être recopiée dans le message.
func ParseAgeRecipient(s string) (r age.Recipient, hybrid bool, err error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(strings.ToUpper(s), "AGE-SECRET-KEY-") || strings.HasPrefix(strings.ToUpper(s), "AGE-PLUGIN-"):
		return nil, false, errors.New("identité privée refusée : seule la clé publique (age1…) vient sur le poste")
	case strings.HasPrefix(s, "-----BEGIN"):
		return nil, false, errors.New("clé privée refusée : seule la clé publique vient sur le poste")
	case strings.HasPrefix(s, "ssh-"):
		r, err = agessh.ParseRecipient(s)
	case strings.HasPrefix(s, "age1"):
		var rs []age.Recipient
		rs, err = age.ParseRecipients(strings.NewReader(s))
		if err == nil && len(rs) == 1 {
			r = rs[0]
			_, hybrid = r.(*age.HybridRecipient)
		}
	default:
		err = errors.New("clé publique age (age1…) ou SSH (ssh-ed25519, ssh-rsa) attendue")
	}
	if err != nil {
		return nil, false, fmt.Errorf("destinataire age illisible (%v) ; clés matérielles (plugins) non prises en charge", err)
	}
	return r, hybrid, nil
}

// validateRecipients vérifie une liste de destinataires d'un type (gpg ou age).
func validateRecipients(typ, field string, list []Recipient, add func(field, format string, a ...any)) {
	seen := map[string]bool{}
	hybrid, classic := false, false
	for i, r := range list {
		f := fmt.Sprintf("%s[%d]", field, i)
		switch typ {
		case PrepareGPG:
			if err := checkGPGRecipient(r); err != nil {
				add(f, "%v", err)
				continue
			}
			fp := NormalizeFingerprint(r.Fingerprint)
			if seen[fp] {
				add(f+".fingerprint", "empreinte déjà déclarée")
			}
			seen[fp] = true
		case PrepareAge:
			if r.Age == "" {
				add(f, "clé age attendue sur une ligne (age1…, ssh-ed25519 …), pas fingerprint et key")
				continue
			}
			_, h, err := ParseAgeRecipient(r.Age)
			if err != nil {
				add(f, "%v", err)
				continue
			}
			hybrid, classic = hybrid || h, classic || !h
			if seen[strings.TrimSpace(r.Age)] {
				add(f, "clé déjà déclarée")
			}
			seen[strings.TrimSpace(r.Age)] = true
		}
	}
	if hybrid && classic {
		add(field, "clés hybrides (age1pq1…) et classiques mêlées : age refuse de chiffrer pour les deux à la fois")
	}
}
