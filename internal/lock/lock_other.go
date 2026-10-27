//go:build !windows

package lock

import (
	"errors"
	"os"
	"syscall"
)

// acquire : verrou flock sur un fichier en 0600. Créé par root (exécution lancée avec
// sudo), il est donné au propriétaire du dossier de données, pour que le service
// (compte kura-agent sous Linux) puisse encore l'ouvrir.
func acquire(path string) (Release, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil && !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New(path + " n'est pas un fichier ordinaire")
	}
	if os.Geteuid() == 0 {
		if dir, err := os.Stat(dirOf(path)); err == nil {
			if sys, ok := dir.Sys().(*syscall.Stat_t); ok && sys.Uid != 0 {
				_ = f.Chown(int(sys.Uid), int(sys.Gid))
			}
		}
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

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
