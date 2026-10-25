// Package creds est le coffre de secrets de kura-agent : jetons et URL secrètes
// référencés dans la configuration sous la forme credential:<nom>.
//
// Sous Windows, chaque secret est chiffré par DPAPI dans la portée du compte qui
// l'enregistre : seul ce compte (svc-kura, ou celui qui exécute la tâche) peut le
// relire. Il faut donc lancer `kura-agent credentials set` sous ce même compte.
//
// Hors Windows (développement uniquement), le secret est stocké en clair, lisible
// par le seul propriétaire du fichier.
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

// Store est un coffre rangé dans un dossier.
type Store struct{ Dir string }

func (s Store) path(name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("nom de secret %q invalide (minuscules, chiffres et '-')", name)
	}
	return filepath.Join(s.Dir, name+ext), nil
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
	enc, err := protect([]byte(value))
	if err != nil {
		return fmt.Errorf("chiffrement du secret : %w", err)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
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
	enc, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%q : %w (kura-agent credentials set %s)", name, ErrNotFound, name)
	}
	if err != nil {
		return "", err
	}
	v, err := unprotect(enc)
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
		if n, ok := strings.CutSuffix(e.Name(), ext); ok && !e.IsDir() {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
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
