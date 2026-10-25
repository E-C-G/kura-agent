//go:build windows

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// shutdownSignals : seul Ctrl+C (ou Ctrl+Break) annule une exécution. Go traduit aussi
// la déconnexion d'un utilisateur en SIGTERM ; on ne l'écoute donc pas sous Windows.
func shutdownSignals() []os.Signal { return []os.Signal{os.Interrupt} }

// ignoreLogoff : Windows envoie CTRL_LOGOFF_EVENT à tous les processus console quand
// N'IMPORTE QUEL utilisateur se déconnecte, y compris à ceux de la session 0. Sans ce
// gestionnaire, la déconnexion d'un utilisateur au milieu de la nuit pourrait arrêter la sauvegarde.
// Installé après celui du runtime Go, il est appelé en premier.
func ignoreLogoff() {
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
	if proc.Find() != nil {
		return
	}
	cb := syscall.NewCallback(func(ctrl uint32) uintptr {
		if ctrl == windows.CTRL_LOGOFF_EVENT {
			return 1 // traité : rien d'autre ne se passe
		}
		return 0 // laisser la main au gestionnaire suivant (celui de Go)
	})
	proc.Call(cb, 1)
}
