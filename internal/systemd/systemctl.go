package systemd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Manager installe et retire les unités. Run lance systemctl (remplaçable dans les
// tests) ; Dir est le dossier des unités (UnitDir par défaut).
type Manager struct {
	Dir string
	Run func(args ...string) error
	// unavailable : systemd absent de ce système ; Install et Remove le renvoient
	// sans rien écrire.
	unavailable error
}

func (m Manager) dir() string {
	if m.Dir == "" {
		return UnitDir
	}
	return m.Dir
}

// Install écrit les deux unités (seulement si elles changent), recharge systemd et
// active le timer. Le service n'est jamais activé seul : seul le timer le lance.
func (m Manager) Install(u Units) error {
	if m.unavailable != nil {
		return m.unavailable
	}
	changed := false
	for name, content := range map[string]string{ServiceName: u.Service, TimerName: u.Timer} {
		c, err := writeIfChanged(filepath.Join(m.dir(), name), []byte(content))
		if err != nil {
			return fmt.Errorf("%s : %w", name, err)
		}
		changed = changed || c
	}
	if changed {
		if err := m.Run("daemon-reload"); err != nil {
			return err
		}
	}
	if err := m.Run("enable", TimerName); err != nil {
		return err
	}
	// restart : un horaire modifié est pris en compte tout de suite.
	return m.Run("restart", TimerName)
}

// Remove désactive le timer et retire les unités. Absentes : rien à faire.
func (m Manager) Remove() error {
	if m.unavailable != nil {
		return m.unavailable
	}
	var errs []error
	if m.Exists() {
		if err := m.Run("disable", "--now", TimerName); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range []string{TimerName, ServiceName} {
		if err := os.Remove(filepath.Join(m.dir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := m.Run("daemon-reload"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Exists indique si le timer est installé.
func (m Manager) Exists() bool {
	_, err := os.Stat(filepath.Join(m.dir(), TimerName))
	return err == nil
}

func writeIfChanged(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}
