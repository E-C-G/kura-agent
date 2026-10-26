//go:build !windows

package tray

import (
	"fmt"
	"os"
)

// Main n'existe que sous Windows.
func Main(Options) int {
	fmt.Fprintln(os.Stderr, "l'icône n'existe que sous Windows")
	return 2
}
