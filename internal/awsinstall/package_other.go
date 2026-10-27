//go:build !windows && !linux && !darwin

package awsinstall

// SystemPackage : installation automatique pas encore écrite pour ce système
// (Ensure renvoie une platform.NotImplementedError).
func SystemPackage() Package { return Package{} }

// DefaultPath : aucun emplacement connu pour ce système.
func DefaultPath() string { return "" }
