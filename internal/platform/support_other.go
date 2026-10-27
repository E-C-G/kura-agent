//go:build !windows && !linux && !darwin

package platform

// Autres systèmes : aucune fonction système écrite pour l'instant ; le cœur commun
// compile et ses tests passent.
var implemented = map[Feature]bool{}
