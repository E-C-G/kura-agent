//go:build !windows && !linux

package platform

// macOS et autres : aucune fonction système écrite pour l'instant ; le cœur commun
// compile et ses tests passent.
var implemented = map[Feature]bool{}
