//go:build windows

package prepare

import (
	"fmt"

	"github.com/E-C-G/kura-agent/internal/secdir"
)

// stagingSDDL : SYSTEM et Administrateurs seuls, protégé (le dossier de données donne la
// lecture aux utilisateurs : un export ne doit pas en hériter).
const stagingSDDL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// EnsureStaging crée la zone de préparation ou vérifie celle qui existe (ni jonction ni
// dossier préparé par un autre compte) puis lui pose ses droits (internal/secdir).
func EnsureStaging(dir string) error {
	if err := secdir.Secure(dir, stagingSDDL); err != nil {
		return fmt.Errorf("zone de préparation : %w", err)
	}
	return nil
}
