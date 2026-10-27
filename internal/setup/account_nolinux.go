//go:build !linux

package setup

// EnsureServiceAccount : compte de service propre à Linux.
func EnsureServiceAccount() (bool, error) { return false, notImplemented() }
