//go:build !linux && !windows

package prepare

// extendedACL : macOS, les ACL ne se lisent pas sans cgo ; les dossiers système de root
// n'en portent pas. Portage macOS en pause : à reprendre avant sa distribution.
func extendedACL(string) bool { return false }
