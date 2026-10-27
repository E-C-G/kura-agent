//go:build !windows

package awsinstall

import (
	"fmt"
	"os"
)

// secureWorkDir : dossier de travail en 0700, refusé s'il est un lien symbolique.
func secureWorkDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s n'est pas un dossier (lien ?)", dir)
	}
	return os.Chmod(dir, 0o700)
}
