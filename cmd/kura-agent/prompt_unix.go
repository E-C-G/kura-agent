//go:build linux || darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// hideInput coupe l'écho du terminal ; nil si l'entrée n'est pas un terminal (fichier,
// tube). Le terminal est remis en état à la fin, et si la saisie est interrompue.
func hideInput() func() {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return nil
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }
}

// stdinIsTerminal : l'entrée est un terminal (une personne peut répondre).
func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlGetTermios)
	return err == nil
}
