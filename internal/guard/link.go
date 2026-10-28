package guard

import (
	"fmt"
	"path/filepath"
	"strings"
)

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

// LinkUnder dit si le chemin p, sous la source root, passe par un lien symbolique ou une
// jonction (un de ses dossiers entre root et lui, ou lui-même) ; il renvoie alors le
// chemin relatif du lien, en séparateurs /. Sert à contrôler, au fil de la
// synchronisation, chaque fichier envoyé par aws : un lien posé après le parcours
// préalable est ainsi repéré.
func LinkUnder(root, p string) (string, bool) {
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		if link, err := isLink(cur); err == nil && link {
			return strings.Join(parts[:i+1], "/"), true
		}
	}
	return "", false
}

// ExcludePattern : motif --exclude de aws (fnmatch, relatif à la source) qui écarte un
// lien relevé et tout ce qui passe par lui. Les caractères spéciaux de fnmatch dans le
// nom ([, *, ?) sont neutralisés.
func ExcludePattern(rel string) []string {
	esc := strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]").Replace(rel)
	return []string{esc, esc + "/*"}
}
