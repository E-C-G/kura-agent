// Package lock empêche deux exécutions simultanées de run sur la même machine.
//
// Le verrou est un fichier du dossier de données (run.lock), que seuls l'agent et les
// administrateurs peuvent ouvrir : un utilisateur ordinaire ne peut pas le tenir pour
// faire croire à une exécution en cours et bloquer les sauvegardes (ce que permettait
// l'ancien mutex Global\Kura-Run, créable par tout compte, ou un fichier de /tmp).
// Le verrou disparaît avec le processus qui le tient.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/E-C-G/kura-agent/internal/paths"
)

// ErrBusy signale qu'une exécution est déjà en cours.
var ErrBusy = errors.New("une exécution est déjà en cours")

// FileName : fichier du verrou dans le dossier de données.
const FileName = "run.lock"

// Release libère le verrou.
type Release func()

// Acquire prend le verrou ou renvoie ErrBusy. Le dossier de données doit exister (il
// est créé et protégé par install).
func Acquire() (Release, error) { return AcquireIn(paths.DataDir()) }

// AcquireIn prend le verrou du dossier dir.
func AcquireIn(dir string) (Release, error) {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("verrou : dossier %s absent (kura-agent install le crée)", dir)
	}
	return acquire(filepath.Join(dir, FileName))
}
