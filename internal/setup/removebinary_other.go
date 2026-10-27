//go:build !linux && !darwin

package setup

// RemoveBinary : propre à Linux et macOS (exécutable copié dans /usr/local/bin).
func RemoveBinary() (string, error) { return "", notImplemented() }
