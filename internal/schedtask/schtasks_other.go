//go:build !windows

package schedtask

import "errors"

var errUnsupported = errors.New("tâche planifiée : Windows uniquement")

// Register n'existe que sous Windows.
func Register(string) error { return errUnsupported }

// Exists n'existe que sous Windows.
func Exists() bool { return false }

// Delete n'existe que sous Windows.
func Delete() error { return errUnsupported }
