// Package paths centralise les emplacements de kura-agent sur disque.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// DataDir est C:\ProgramData\Kura sous Windows.
// KURA_DATA_DIR le remplace (tests, développement).
func DataDir() string {
	if d := os.Getenv("KURA_DATA_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "Kura")
	}
	return "/var/lib/kura"
}

// ConfigFile est le fichier de configuration par défaut.
func ConfigFile() string { return filepath.Join(DataDir(), "config.yaml") }

// StatusFile est l'état courant, relu par les icônes.
func StatusFile() string { return filepath.Join(DataDir(), "status.json") }

// HistoryFile garde une ligne JSON par exécution.
func HistoryFile() string { return filepath.Join(DataDir(), "history.jsonl") }

// RunsDir contient un dossier de journaux par exécution.
func RunsDir() string { return filepath.Join(DataDir(), "logs", "runs") }

// CredentialsDir contient le coffre de secrets (chiffrés par DPAPI sous Windows).
func CredentialsDir() string { return filepath.Join(DataDir(), "credentials") }

// PendingDir garde les bilans webhook non envoyés, renvoyés à l'exécution suivante.
func PendingDir() string { return filepath.Join(DataDir(), "pending") }
