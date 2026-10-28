//go:build !windows

package restore

import (
	"io/fs"
	"syscall"
)

// fileOwner : propriétaire et groupe d'un fichier.
func fileOwner(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
