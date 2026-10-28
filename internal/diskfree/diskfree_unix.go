//go:build !windows

package diskfree

import "golang.org/x/sys/unix"

// Free : octets disponibles pour l'administrateur sur le volume de dir.
func Free(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
