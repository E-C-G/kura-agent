//go:build !windows

package secdir

import "os"

// Secure : hors Windows, crée le dossier en 0700 ; les dossiers de l'agent sont
// protégés par leurs droits Unix (internal/setup, internal/creds).
func Secure(dir, _ string) error { return os.MkdirAll(dir, 0o700) }
