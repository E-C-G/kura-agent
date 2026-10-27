//go:build !windows && !linux && !darwin

package main

// hideInput : autres systèmes (développement), la saisie reste visible.
func hideInput() func() { return nil }
