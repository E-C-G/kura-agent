package prepare

import (
	"fmt"
	"os"
	"path/filepath"
)

// CheckTool vérifie qu'un outil peut être lancé avec les droits de l'agent (SYSTEM,
// root) : chemin absolu, fichier ordinaire présent, et ni lui ni ses dossiers parents
// modifiables par un autre compte que les administrateurs. Sinon un utilisateur du poste
// remplacerait l'outil (ou poserait une DLL à côté) et ferait exécuter son code par l'agent.
func CheckTool(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q : chemin absolu attendu", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s introuvable : installer l'outil, ou corriger son chemin dans tools (%v)", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s n'est pas un fichier", path)
	}
	return adminOnly(path)
}
