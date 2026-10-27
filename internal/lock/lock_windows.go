//go:build windows

package lock

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// lockSDDL : le fichier n'est ouvrable que par SYSTEM et les Administrateurs ; un
// handle sans accès aux données (attributs seuls) ne bloque pas le partage.
const lockSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"

// acquire ouvre run.lock sans partage : tant que le handle existe, toute autre
// ouverture échoue (ERROR_SHARING_VIOLATION). Windows ferme le handle si le processus
// meurt.
func acquire(path string) (Release, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(lockSDDL)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, sa,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	switch {
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return nil, ErrBusy
	case err != nil:
		return nil, fmt.Errorf("verrou %s : %w", path, err)
	}
	return func() { windows.CloseHandle(h) }, nil
}
