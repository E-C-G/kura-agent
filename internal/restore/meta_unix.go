//go:build !windows

package restore

import (
	"errors"
	"os"
	"syscall"
)

// setPrivate : sans objet ici (propriétaire root et mode 0600 donnés par Owner).
func setPrivate(string) error { return nil }

// copySecurity donne au fichier to le mode et le propriétaire de from. Sans droits root,
// le changement de propriétaire est omis (développement).
func copySecurity(from, to string) error {
	fi, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if err := os.Chmod(to, fi.Mode().Perm()); err != nil {
		return err
	}
	if uid, gid, ok := fileOwner(fi); ok {
		if err := os.Lchown(to, uid, gid); err != nil && !(errors.Is(err, syscall.EPERM) && os.Geteuid() != 0) {
			return err
		}
	}
	return nil
}
