//go:build !windows

package config

import "os/user"

// Hors Windows (développement, kura-ctl sous macOS/Linux) : dossier personnel du compte.
func (systemEnv) ProfileDir(account string) (string, error) {
	u, err := user.Lookup(account)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

func (systemEnv) AccountExists(account string) bool {
	_, err := user.Lookup(account)
	return err == nil
}
