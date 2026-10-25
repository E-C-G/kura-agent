//go:build windows

package state

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// rename remplace dst par src en réessayant quelques fois : sous Windows, le
// remplacement échoue tant qu'un lecteur (icône, antivirus, indexeur) tient dst
// ouvert sans partage de suppression.
func rename(src, dst string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		time.Sleep(time.Duration(25*(i+1)) * time.Millisecond)
	}
	return err
}

// readFile lit path en autorisant les autres à le remplacer pendant la lecture
// (FILE_SHARE_DELETE), pour ne jamais bloquer l'écriture de run.
func readFile(path string) ([]byte, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, os.ErrNotExist
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	return io.ReadAll(f)
}
