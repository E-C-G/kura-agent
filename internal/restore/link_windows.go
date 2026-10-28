package restore

import (
	"errors"

	"golang.org/x/sys/windows"
)

// linkUnsupported : le volume ne connaît pas les liens physiques (FAT, exFAT, partage).
func linkUnsupported(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}
