package launchd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Manager installe et retire les démons. Run lance launchctl (remplaçable dans les
// tests) ; Dir est le dossier des démons (DaemonDir par défaut).
type Manager struct {
	Dir string
	Run func(args ...string) error
	// unavailable : launchd absent de ce système ; Install et Remove le renvoient
	// sans rien écrire.
	unavailable error
}

func (m Manager) dir() string {
	if m.Dir == "" {
		return DaemonDir
	}
	return m.Dir
}

func (m Manager) path(label string) string { return filepath.Join(m.dir(), PlistName(label)) }

// Install écrit les démons et les (re)charge : un horaire modifié est pris en compte
// tout de suite. Sans rattrapage, un ancien démon de rattrapage est retiré.
func (m Manager) Install(p Plists) error {
	if m.unavailable != nil {
		return m.unavailable
	}
	if len(p.Sync) == 0 {
		return errors.New("démon de sauvegarde vide")
	}
	for _, d := range []struct {
		label string
		data  []byte
	}{{Label, p.Sync}, {CatchUpLabel, p.CatchUp}} {
		if len(d.data) == 0 {
			if err := m.remove(d.label); err != nil {
				return err
			}
			continue
		}
		if err := writeFile(m.path(d.label), d.data); err != nil {
			return fmt.Errorf("%s : %w", PlistName(d.label), err)
		}
		// Déjà chargé : déchargé d'abord (erreur ignorée s'il ne l'était pas).
		_ = m.Run("bootout", "system/"+d.label)
		if err := m.Run("enable", "system/"+d.label); err != nil {
			return err
		}
		if err := m.Run("bootstrap", "system", m.path(d.label)); err != nil {
			return err
		}
	}
	return nil
}

func (m Manager) remove(label string) error {
	if _, err := os.Stat(m.path(label)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_ = m.Run("bootout", "system/"+label)
	if err := os.Remove(m.path(label)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Remove décharge et retire les démons. Absents : rien à faire.
func (m Manager) Remove() error {
	if m.unavailable != nil {
		return m.unavailable
	}
	return errors.Join(m.remove(CatchUpLabel), m.remove(Label))
}

// Exists indique si le démon de sauvegarde est installé.
func (m Manager) Exists() bool {
	_, err := os.Stat(m.path(Label))
	return err == nil
}

// InstalledAt : date de mise en place de la planification (écriture du démon de
// sauvegarde) ; zéro s'il est absent. Un passage antérieur n'est pas à rattraper.
func (m Manager) InstalledAt() time.Time {
	st, err := os.Stat(m.path(Label))
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// writeFile écrit le démon, lisible par tous et modifiable par son propriétaire seul
// (launchd refuse un démon modifiable par d'autres).
func writeFile(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
