//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// hideInput coupe l'écho de la console ; nil si l'entrée n'est pas une console.
func hideInput() func() {
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return nil
	}
	if windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT) != nil {
		return nil
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }
}
