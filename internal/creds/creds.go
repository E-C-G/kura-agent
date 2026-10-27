// Package creds est le coffre de secrets de kura-agent : jetons et URL secrètes
// référencés dans la configuration sous la forme credential:<nom>.
//
// Sous Windows, chaque secret est chiffré par DPAPI dans la portée de la machine, et
// le dossier n'est lisible que par SYSTEM et les Administrateurs (ACL protégée, posée
// à chaque écriture). `kura-agent credentials set` se lance donc depuis une console
// d'administration, et la tâche (SYSTEM) relit les mêmes secrets.
//
// Sous Linux (System), chaque secret est chiffré par systemd-creds (clé de la machine,
// et puce TPM si elle existe) dans /etc/credstore.encrypted/kura.<nom>. Le service
// kura-sync, qui tourne sous le compte kura-agent, ne lit jamais ces fichiers : systemd
// les lui déchiffre au démarrage (ImportCredential=kura.*) dans $CREDENTIALS_DIRECTORY.
// Les commandes lancées avec sudo les déchiffrent elles-mêmes.
//
// Ailleurs, et avec KURA_DEV=1 (développement uniquement), le secret est stocké en
// clair, lisible par le seul propriétaire du fichier.
package creds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ErrNotFound signale un secret absent du coffre.
var ErrNotFound = errors.New("secret absent du coffre")

var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

const ext = ".secret"

// Store est un coffre rangé dans un dossier. Le zéro (Store{Dir: …}) chiffre comme
// le système de compilation le fait par défaut (DPAPI sous Windows, en clair
// ailleurs) ; System renvoie le coffre de production de chaque système.
type Store struct {
	Dir string
	// cipher : chiffrement propre au système (nil : protect / unprotect).
	cipher cipher
	// prefix : nom de fichier prefix+nom (Linux : « kura. ») ; vide : nom+".secret".
	prefix string
	// serviceDir : secrets déjà déchiffrés par systemd pour le service
	// ($CREDENTIALS_DIRECTORY), lus en priorité.
	serviceDir string
}

// cipher chiffre un secret ; name est le nom complet du fichier (vérifié au
// déchiffrement par systemd-creds).
type cipher interface {
	protect(name string, plain []byte) ([]byte, error)
	unprotect(name string, enc []byte) ([]byte, error)
}

func (s Store) fileName(name string) string {
	if s.prefix != "" {
		return s.prefix + name
	}
	return name + ext
}

func (s Store) path(name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("nom de secret %q invalide (minuscules, chiffres et '-')", name)
	}
	return filepath.Join(s.Dir, s.fileName(name)), nil
}

func (s Store) protect(name string, plain []byte) ([]byte, error) {
	if s.cipher != nil {
		return s.cipher.protect(s.fileName(name), plain)
	}
	return protect(plain)
}

func (s Store) unprotect(name string, enc []byte) ([]byte, error) {
	if s.cipher != nil {
		return s.cipher.unprotect(s.fileName(name), enc)
	}
	return unprotect(enc)
}

// Init crée le dossier du coffre et lui applique son ACL (aussi fait à chaque Set).
func (s Store) Init() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return secureDir(s.Dir)
}

// Set enregistre (ou remplace) un secret.
func (s Store) Set(name, value string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if value == "" {
		return errors.New("secret vide")
	}
	enc, err := s.protect(name, []byte(value))
	if err != nil {
		return fmt.Errorf("chiffrement du secret : %w", err)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	// Jamais de secret dans un dossier dont on n'a pas pu restreindre l'accès.
	if err := secureDir(s.Dir); err != nil {
		return fmt.Errorf("protection du dossier %s : %w", s.Dir, err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Get relit un secret.
func (s Store) Get(name string) (string, error) {
	p, err := s.path(name)
	if err != nil {
		return "", err
	}
	if s.serviceDir != "" {
		// Service systemd : secret déjà déchiffré, lisible par lui seul.
		if v, err := os.ReadFile(filepath.Join(s.serviceDir, s.fileName(name))); err == nil {
			return string(v), nil
		}
	}
	enc, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%q : %w (kura-agent credentials set %s)", name, ErrNotFound, name)
	}
	if err != nil {
		return "", err
	}
	v, err := s.unprotect(name, enc)
	if err != nil {
		return "", fmt.Errorf("%q illisible : enregistré sous un autre compte ? (%w)", name, err)
	}
	return string(v), nil
}

// Delete supprime un secret ; absent n'est pas une erreur.
func (s Store) Delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List renvoie les noms des secrets enregistrés, sans leur valeur.
func (s Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var n string
		var ok bool
		if s.prefix != "" {
			n, ok = strings.CutPrefix(e.Name(), s.prefix)
		} else {
			n, ok = strings.CutSuffix(e.Name(), ext)
		}
		if ok && namePattern.MatchString(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// AWSKeys est une paire de clés d'accès S3 (Wasabi), rangée dans le coffre sous la
// forme "ID:SECRET" (une clé secrète AWS ou Wasabi ne contient jamais ':').
type AWSKeys struct {
	AccessKeyID     string
	SecretAccessKey string
}

// JoinAWSKeys forme la valeur rangée dans le coffre.
func JoinAWSKeys(id, secret string) (string, error) {
	id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
	if id == "" || secret == "" {
		return "", errors.New("identifiant et clé secrète obligatoires")
	}
	if strings.ContainsAny(id, ": \t") || strings.ContainsAny(secret, ": \t") {
		return "", errors.New("identifiant ou clé secrète invalide (espace ou ':')")
	}
	return id + ":" + secret, nil
}

// ParseAWSKeys relit une valeur "ID:SECRET".
func ParseAWSKeys(v string) (AWSKeys, error) {
	id, secret, ok := strings.Cut(v, ":")
	if !ok || id == "" || secret == "" {
		return AWSKeys{}, errors.New("clés d'accès attendues sous la forme ID:SECRET (kura-agent credentials set-aws NOM)")
	}
	return AWSKeys{AccessKeyID: id, SecretAccessKey: secret}, nil
}

// Env renvoie les variables d'environnement qui donnent ces clés à la CLI AWS ; elles
// priment sur les fichiers ~/.aws. AWS_SESSION_TOKEN est vidé pour ne pas hériter du
// jeton de session d'une console qui lance l'agent à la main.
func (k AWSKeys) Env() []string {
	return []string{"AWS_ACCESS_KEY_ID=" + k.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + k.SecretAccessKey,
		"AWS_SESSION_TOKEN="}
}

// Resolve renvoie la valeur d'un champ de configuration : la valeur telle quelle,
// ou le secret désigné par credential:<nom>.
func (s Store) Resolve(value string) (string, error) {
	name, ok := strings.CutPrefix(value, "credential:")
	if !ok {
		return value, nil
	}
	return s.Get(name)
}
