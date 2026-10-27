//go:build !windows

package platform

// Linux, macOS : aucune fonction système écrite pour l'instant ; le cœur commun
// compile et ses tests passent.
var implemented = map[Feature]bool{}
