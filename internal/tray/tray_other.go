//go:build !windows

package tray

import (
	"fmt"
	"os"

	"github.com/E-C-G/kura-agent/internal/platform"
)

// Main : icône pas encore écrite pour ce système.
func Main(Options) int {
	fmt.Fprintln(os.Stderr, "kura-agent tray :", platform.NotImplemented(platform.Tray))
	return 2
}
