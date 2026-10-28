//go:build !windows

package prepare

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// EnsureStaging crée la zone de préparation (0700, au compte de l'agent) ou vérifie
// celle qui existe : vrai dossier, pas un lien, au compte de l'agent ; ses droits sont
// ramenés à 0700 par son descripteur.
func EnsureStaging(dir string) error {
	err := os.Mkdir(dir, 0o700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("zone de préparation %s : %w", dir, err)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("zone de préparation %s : ni lien ni fichier attendu (%w)", dir, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("zone de préparation %s : %w", dir, err)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("zone de préparation %s : appartient à l'utilisateur %d, pas au compte de l'agent ; la supprimer, elle sera recréée", dir, st.Uid)
	}
	if st.Mode&0o777 != 0o700 {
		if err := unix.Fchmod(fd, 0o700); err != nil {
			return fmt.Errorf("zone de préparation %s : %w", dir, err)
		}
	}
	return nil
}
