//go:build !linux

package setup

// ServiceAccount : sans objet hors Linux (Windows : SYSTEM).
const ServiceAccount = ""

// EnsureServiceAccount : compte de service propre à Linux.
func EnsureServiceAccount() (bool, error) { return false, notImplemented() }
