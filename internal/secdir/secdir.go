// Package secdir crée et protège les dossiers de l'agent sous Windows
// (C:\ProgramData\Kura et son coffre) contre un dossier préparé à l'avance par un
// utilisateur standard, qui peut créer des sous-dossiers dans C:\ProgramData et en
// resterait propriétaire (droits implicites de modifier l'ACL), ou y poser une
// jonction vers un emplacement de son choix.
//
// Secure refuse un point de reparse (jonction, lien) et un dossier dont le propriétaire
// n'est ni SYSTEM, ni le groupe Administrateurs, ni un administrateur ; sinon il pose
// l'ACL protégée demandée et donne le dossier au groupe Administrateurs. Hors Windows,
// les dossiers sont protégés par leurs droits Unix (voir internal/setup).
package secdir

import "fmt"

// ForeignOwnerError : dossier préexistant appartenant à un compte non administrateur.
type ForeignOwnerError struct {
	Dir, Owner string
}

func (e *ForeignOwnerError) Error() string {
	return fmt.Sprintf(`%s appartient à %s, qui n'est pas administrateur : il a été créé avant l'installation par un autre compte, qui pourrait lire les secrets ou modifier la configuration. Le supprimer depuis une invite de commandes administrateur (takeown /f "%s" /r /a /d O puis rmdir /s /q "%s"), puis relancer`,
		e.Dir, e.Owner, e.Dir, e.Dir)
}

// ReparsePointError : le dossier est une jonction ou un lien symbolique.
type ReparsePointError struct{ Dir string }

func (e *ReparsePointError) Error() string {
	return fmt.Sprintf(`%s est une jonction ou un lien, pas un dossier : le supprimer (rmdir "%s" depuis une invite administrateur, la cible n'est pas touchée), puis relancer`, e.Dir, e.Dir)
}
