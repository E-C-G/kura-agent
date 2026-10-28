//go:build !windows

package restore

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// rights : mode, propriétaire et groupe d'un fichier.
type rights struct {
	mode     fs.FileMode
	uid, gid int
	owner    bool
}

// readRights lit les droits d'un fichier ouvert.
func readRights(f *os.File) (rights, error) {
	fi, err := f.Stat()
	if err != nil {
		return rights{}, err
	}
	uid, gid, ok := fileOwner(fi)
	return rights{mode: fi.Mode().Perm(), uid: uid, gid: gid, owner: ok}, nil
}

// applyRights les donne au fichier ouvert (fchmod, fchown). Sans droits root, le
// propriétaire n'est pas changé (développement).
func applyRights(f *os.File, r rights) error {
	if err := f.Chmod(r.mode); err != nil {
		return err
	}
	if r.owner {
		if err := f.Chown(r.uid, r.gid); err != nil && !(errors.Is(err, syscall.EPERM) && os.Geteuid() != 0) {
			return err
		}
	}
	return nil
}

// privateRights : sans objet ici (Owner root et mode 0600 font de même).
func privateRights() rights { return rights{mode: fileMode} }

// setTimes date le fichier ouvert (futimes).
func setTimes(f *os.File, t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Futimes(int(f.Fd()), []unix.Timeval{tv, tv})
}

// stagingDir : sans objet ici (le temporaire, créé en 0600, ne laisse rien lire).
func (d *Dest) stagingDir() (string, error) { return "", nil }

// settle : sans objet ici.
func (d *Dest) settle(string, rights) error { return nil }

// CreatePrivateDir crée un dossier réservé à son propriétaire (0700).
func CreatePrivateDir(p string) error { return os.Mkdir(p, 0o700) }

// SecureExistingDir : sans objet ici (les fichiers restaurés sont eux-mêmes en 0600).
func SecureExistingDir(string) error { return nil }
