//go:build linux

package prepare

import (
	"errors"

	"golang.org/x/sys/unix"
)

// extendedACL : p porte une ACL POSIX (ou on ne peut pas le savoir).
func extendedACL(p string) bool {
	_, err := unix.Lgetxattr(p, "system.posix_acl_access", nil)
	return err == nil || !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP)
}
