//go:build !windows && !linux && !darwin

package setup

// CheckAdmin : installation pas encore écrite pour ce système.
func CheckAdmin() error { return notImplemented() }
