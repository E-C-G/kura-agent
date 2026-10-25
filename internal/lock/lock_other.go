//go:build !windows

package lock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Hors Windows (développement) : verrou de fichier.
func acquire(name string) (Release, error) {
	path := filepath.Join(os.TempDir(), strings.NewReplacer(`\`, "-", "/", "-").Replace(name)+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
