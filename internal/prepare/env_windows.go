//go:build windows

package prepare

import (
	"os"
	"path/filepath"
)

// toolEnv : environnement réduit de l'outil (aucune variable de l'agent), fichiers
// temporaires dans tmp.
func toolEnv(tmp string) []string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return []string{
		"SystemRoot=" + root, "windir=" + root, "SystemDrive=" + filepath.VolumeName(root),
		"PATH=" + filepath.Join(root, "System32") + ";" + root,
		"TEMP=" + tmp, "TMP=" + tmp,
	}
}
