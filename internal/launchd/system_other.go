//go:build !darwin

package launchd

import "github.com/E-C-G/kura-agent/internal/platform"

// System : launchd n'existe que sous macOS.
func System() Manager { return Manager{unavailable: platform.NotImplemented(platform.Scheduling)} }
