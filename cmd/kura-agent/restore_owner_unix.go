//go:build !windows

package main

import (
	"os"

	"github.com/E-C-G/kura-agent/internal/restore"
)

// destOwner : dans un dossier à part, les fichiers restaurés appartiennent à root, en
// 0600 (dossiers 0700) : la restauration peut mêler les fichiers de tous les comptes,
// le bucket ne garde pas leurs droits d'origine. L'administrateur les remet ensuite à
// leur propriétaire (chown) ou restaure en place. Sans droits root, rien n'est changé
// (développement).
func destOwner(string) (restore.Owner, error) {
	return restore.Owner{Set: os.Geteuid() == 0, UID: 0, GID: 0}, nil
}

// inPlaceOwner : en place, chaque fichier créé prend le propriétaire et le groupe du
// dossier qui le reçoit, en 0600 ; un fichier remplacé garde ceux de l'actuel.
func inPlaceOwner() restore.Owner { return restore.Owner{Set: os.Geteuid() == 0, Inherit: true} }
