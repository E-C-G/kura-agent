//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/E-C-G/kura-agent/internal/restore"
)

// destOwner : les fichiers restaurés prennent le propriétaire et le groupe du dossier qui
// les reçoit, pour qu'une restauration faite par root reste lisible par l'utilisateur.
// Sans droits root, rien n'est changé (développement).
func destOwner(dir string) (restore.Owner, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return restore.Owner{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return restore.Owner{}, fmt.Errorf("%s : propriétaire illisible", dir)
	}
	return restore.Owner{Set: os.Geteuid() == 0, UID: int(st.Uid), GID: int(st.Gid)}, nil
}

// inPlaceOwner : en place, chaque fichier prend le propriétaire et le groupe du dossier
// qui le reçoit (sources de comptes différents).
func inPlaceOwner() restore.Owner { return restore.Owner{Set: os.Geteuid() == 0, Inherit: true} }
