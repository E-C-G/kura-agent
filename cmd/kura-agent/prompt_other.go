//go:build !windows

package main

// hideInput : hors Windows (développement), la saisie reste visible.
func hideInput() func() { return nil }
