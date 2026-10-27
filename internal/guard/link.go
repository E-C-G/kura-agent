package guard

import "fmt"

// LinkError : la source d'un job est elle-même un lien symbolique ou une jonction.
// Refusée : un utilisateur qui peut remplacer le dossier par un lien ferait sauvegarder
// (et envoyer au bucket) un emplacement de son choix, que l'agent sait lire (SYSTEM,
// root, lecture de tout le disque sous Linux).
type LinkError struct{ Source string }

func (e *LinkError) Error() string {
	return fmt.Sprintf("source %q : lien symbolique ou jonction, refusée ; indiquer le dossier réel dans la configuration", e.Source)
}

// CheckSourceNotLink renvoie une *LinkError si source est un lien ou une jonction.
func CheckSourceNotLink(source string) error {
	link, err := isLink(source)
	if err != nil {
		return err
	}
	if link {
		return &LinkError{Source: source}
	}
	return nil
}
