//go:build !windows

package prepare

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// adminOnly : l'outil et chacun de ses dossiers parents appartiennent à root, ne sont
// modifiables ni par leur groupe ni par les autres, et ne portent pas d'ACL étendue (qui
// donnerait l'écriture sans que les droits Unix le montrent). Un lien est suivi : sa cible
// et ses dossiers sont vérifiés à leur tour.
func adminOnly(path string) error {
	if err := chainOwnedByRoot(path); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if real != path {
		return chainOwnedByRoot(real)
	}
	return nil
}

func chainOwnedByRoot(path string) error {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s : propriétaire illisible", p)
		}
		if st.Uid != 0 {
			return fmt.Errorf("%s appartient à l'utilisateur %d, pas à root : un autre compte pourrait remplacer l'outil ; le mettre dans un dossier de root (ex. /usr/local/libexec/kura)", p, st.Uid)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			if perm := fi.Mode().Perm(); perm&0o022 != 0 {
				return fmt.Errorf("%s est modifiable par d'autres comptes que root (droits %v) : un autre compte pourrait remplacer l'outil", p, fi.Mode())
			}
			if extendedACL(p) {
				return fmt.Errorf("%s porte une ACL étendue : elle peut donner l'écriture à d'autres comptes ; la retirer (setfacl -b)", p)
			}
		}
		if parent := filepath.Dir(p); parent == p {
			return nil
		}
	}
}
