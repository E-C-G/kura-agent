package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Quarantaine : préfixes du bucket envoyés à travers un lien ou une jonction apparu
// pendant une sauvegarde. Leur contenu ne vient pas de la source (coffre de l'agent,
// profil d'un autre compte) : la restauration depuis l'icône ne les sert jamais.

// QuarantineEntry : un préfixe et la date où il a été relevé.
type QuarantineEntry struct {
	Prefix string    `json:"prefix"`
	At     time.Time `json:"at"`
}

// ReadQuarantine lit la liste ; absente : vide.
func ReadQuarantine(path string) ([]QuarantineEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var q struct {
		Entries []QuarantineEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, err
	}
	return q.Entries, nil
}

// AddQuarantine ajoute des préfixes (sans doublon).
func AddQuarantine(path string, prefixes []string, now time.Time) error {
	entries, err := ReadQuarantine(path)
	if err != nil {
		// Fichier abîmé : jamais réécrit (ses entrées seraient perdues). Les
		// restaurations refusent tant qu'il est illisible ; à corriger par un
		// administrateur.
		return fmt.Errorf("%s illisible, préfixes %s non ajoutés : %w", path, strings.Join(prefixes, ", "), err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Prefix] = true
	}
	for _, p := range prefixes {
		if p = strings.Trim(p, "/"); p != "" && !seen[p] {
			seen[p] = true
			entries = append(entries, QuarantineEntry{Prefix: p, At: now})
		}
	}
	return WriteAtomic(path, struct {
		Entries []QuarantineEntry `json:"entries"`
	}{entries})
}

// Quarantined dit si une clé relève d'un préfixe en quarantaine.
func Quarantined(entries []QuarantineEntry, key string) bool {
	for _, e := range entries {
		if key == e.Prefix || strings.HasPrefix(key, e.Prefix+"/") {
			return true
		}
	}
	return false
}
