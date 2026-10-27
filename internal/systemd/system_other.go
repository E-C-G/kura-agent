//go:build !linux

package systemd

import "github.com/E-C-G/kura-agent/internal/platform"

// System : systemd n'existe que sous Linux.
func System() Manager { return Manager{unavailable: platform.NotImplemented(platform.Scheduling)} }
