//go:build windows

package lock

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// acquire crée le mutex nommé. Le verrou est tenu tant que le handle existe :
// on teste l'existence de l'objet plutôt que sa possession, ce qui évite les
// contraintes de thread propriétaire des mutex Windows avec les goroutines.
// Si le processus meurt, Windows ferme le handle et le verrou disparaît.
//
// Seul `run` doit ouvrir ce mutex : un autre processus qui en garderait un handle
// (l'icône, par exemple) ferait croire à une exécution en cours.
func acquire(name string) (Release, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutex(nil, false, p)
	switch {
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS),
		// Mutex créé par un autre compte : il existe, donc une exécution est en cours.
		errors.Is(err, windows.ERROR_ACCESS_DENIED):
		if h != 0 {
			windows.CloseHandle(h)
		}
		return nil, ErrBusy
	case err != nil:
		return nil, fmt.Errorf("création du verrou %s : %w", name, err)
	}
	return func() { windows.CloseHandle(h) }, nil
}
