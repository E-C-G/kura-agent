//go:build !windows

package guard

import "os"

func isLink(p string) (bool, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return false, err
	}
	return st.Mode()&os.ModeSymlink != 0, nil
}
