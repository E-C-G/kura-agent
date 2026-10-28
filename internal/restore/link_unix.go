//go:build !windows

package restore

import (
	"errors"
	"syscall"
)

// linkUnsupported : le système de fichiers ne connaît pas les liens physiques (vfat,
// exfat : EPERM ; certains partages : EOPNOTSUPP).
func linkUnsupported(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOTSUP)
}
