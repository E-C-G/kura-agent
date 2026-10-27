//go:build windows

package awsinstall

import "github.com/E-C-G/kura-agent/internal/secdir"

// secureWorkDir : dossier de travail réservé à SYSTEM et aux Administrateurs, refusé
// s'il a été préparé par un autre compte (jonction, propriétaire) : le paquet vérifié
// y reste jusqu'à son installation.
func secureWorkDir(dir string) error {
	return secdir.Secure(dir, "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
}
