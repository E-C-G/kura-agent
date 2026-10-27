//go:build windows

package creds

import "github.com/E-C-G/kura-agent/internal/secdir"

// dirSDDL : SYSTEM et Administrateurs seuls, ACL protégée (rien n'est hérité de
// C:\ProgramData, où les Utilisateurs peuvent lire et créer).
const dirSDDL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// secureDir refuse un coffre préparé à l'avance par un autre compte (jonction,
// propriétaire non administrateur), puis pose dirSDDL et le propriétaire
// Administrateurs (internal/secdir).
func secureDir(dir string) error { return secdir.Secure(dir, dirSDDL) }
