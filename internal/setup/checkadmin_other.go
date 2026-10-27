//go:build !windows && !linux

package setup

// CheckAdmin : installation pas encore écrite pour ce système.
func CheckAdmin() error { return notImplemented() }
