//go:build !linux

package setup

// ServiceAccount : sans objet hors Linux (Windows : SYSTEM).
const ServiceAccount = ""

// EnsureServiceAccount : compte de service propre à Linux.
func EnsureServiceAccount() (bool, error) { return false, notImplemented() }

// RemoveBinary : propre à Linux (exécutable copié dans /usr/local/bin).
func RemoveBinary() (string, error) { return "", notImplemented() }
