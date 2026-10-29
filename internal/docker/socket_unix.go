//go:build !windows

package docker

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// CheckSocket vérifie le socket de l'API avant de s'y connecter : un vrai socket, ni
// lien ni fichier ordinaire, à root, au groupe docker (ou root), modifiable par personne
// d'autre. Un faux socket posé par un utilisateur recevrait sinon les demandes de gel,
// et lui ferait croire l'agent sur l'état des conteneurs.
func CheckSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("socket de l'API Docker %s : %w (Docker Engine installé et démarré ?)", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s n'est pas un socket (lien ou fichier refusé)", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s : propriétaire illisible", path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("%s appartient à l'utilisateur %d, pas à root : refusé", path, st.Uid)
	}
	if st.Gid != 0 {
		g, err := user.LookupGroupId(strconv.Itoa(int(st.Gid)))
		if err != nil || g.Name != "docker" {
			return fmt.Errorf("%s appartient au groupe %d, ni root ni docker : refusé", path, st.Gid)
		}
	}
	if fi.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%s est modifiable par tous les comptes : refusé", path)
	}
	return nil
}
