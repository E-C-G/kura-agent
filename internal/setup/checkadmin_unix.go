//go:build linux || darwin

package setup

import (
	"errors"
	"os"
)

// CheckAdmin : installation et coffre demandent root (sudo).
func CheckAdmin() error {
	if os.Geteuid() != 0 {
		return errors.New("droits d'administrateur nécessaires : relancer avec sudo")
	}
	return nil
}
