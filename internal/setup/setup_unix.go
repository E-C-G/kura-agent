//go:build linux || darwin

package setup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/E-C-G/kura-agent/internal/paths"
)

// Linux et macOS : installation dans les emplacements standards ; pas d'icône pour
// l'instant.
//
//	Linux  /etc/kura                configuration (root, lisible par kura-agent)
//	       /var/lib/kura            état (le service la reprend à son compte au démarrage)
//	       /etc/credstore.encrypted secrets chiffrés par systemd-creds (root seul)
//	macOS  /Library/Application Support/Kura  configuration et état (root, lisibles par tous)
//	       …/Kura/credentials       secrets chiffrés, clé dans le trousseau Système (root seul)
//	       /Library/Logs/Kura       journaux
//	tous   /usr/local/bin/kura-agent exécutable copié par install, sauf si kura-agent est
//	                         déjà dans /usr/bin (paquet deb) ou /usr/local/bin

// GUIName : pas de version sans console sous Linux et macOS.
const GUIName = ""

// LocalAccountSID : sans objet sous Linux (ancien compte svc-kura de Windows).
func LocalAccountSID(string) (string, error) { return "", notImplemented() }

// AccountSID : sans objet sous Linux.
func AccountSID(string) (string, error) { return "", notImplemented() }

// RemoveAccount : sans objet sous Linux.
func RemoveAccount(string) error { return notImplemented() }

// SecureDataDir crée les dossiers de l'agent : données en 0755 (aucun secret, lisibles
// par tous comme sous Windows), configuration en 0755, appartenant à root ; sous macOS
// aussi les journaux (sous Linux, systemd les crée pour le service).
func SecureDataDir(dir string) error {
	dirs := []string{dir, paths.ConfigDir()}
	if runtime.GOOS == "darwin" {
		dirs = append(dirs, paths.LogsDir())
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ProgramDir : pas de dossier propre à l'agent sous Linux et macOS (exécutable dans BinDir).
func ProgramDir() string { return "" }

// InstallBinary : exécutable déjà dans /usr/bin ou /usr/local/bin, gardé tel quel ;
// sinon copié dans BinDir. gui toujours vide (pas d'icône).
func InstallBinary(string) (exe, gui string, err error) {
	src, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if src, err = filepath.EvalSymlinks(src); err != nil {
		return "", "", err
	}
	return installBinaryFrom(src, BinDir)
}

func installBinaryFrom(src, binDir string) (string, string, error) {
	switch filepath.Dir(src) {
	case "/usr/bin", binDir:
		return src, "", nil
	}
	dst := filepath.Join(binDir, "kura-agent")
	if err := copyExecutable(src, dst); err != nil {
		return "", "", err
	}
	return dst, "", nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Renommage : un exécutable en cours d'utilisation est remplacé sans erreur.
	return os.Rename(tmp, dst)
}

// RemoveBinary retire l'exécutable copié par install (jamais celui d'un paquet deb,
// dans /usr/bin). removed : chemin supprimé, vide s'il n'y avait rien.
func RemoveBinary() (removed string, err error) {
	p := filepath.Join(BinDir, "kura-agent")
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("%s : %w", p, err)
	}
	return p, nil
}

// SetTrayAutostart : icône pas encore écrite sous Linux et macOS.
func SetTrayAutostart(string) error { return notImplemented() }

// RemoveTrayAutostart : rien à retirer sous Linux pour l'instant.
func RemoveTrayAutostart() error { return nil }

// StopTrays : aucune icône sous Linux pour l'instant.
func StopTrays() int { return 0 }

// RemoveProgramDir : sans objet sous Linux (ProgramDir vide).
func RemoveProgramDir(string) (bool, error) { return false, notImplemented() }
