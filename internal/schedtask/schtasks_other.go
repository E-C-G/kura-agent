//go:build !windows

package schedtask

import "github.com/E-C-G/kura-agent/internal/platform"

// Register : planification pas encore écrite pour ce système.
func Register(string) error { return platform.NotImplemented(platform.Scheduling) }

// Exists : planification pas encore écrite pour ce système.
func Exists() bool { return false }

// Delete : planification pas encore écrite pour ce système.
func Delete() error { return platform.NotImplemented(platform.Scheduling) }
