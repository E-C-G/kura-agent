// Package paths centralise les emplacements de kura-agent sur disque.
//
//	              données                          configuration   journaux
//	Windows       C:\ProgramData\Kura              (données)       (données)\logs
//	Linux         /var/lib/kura                    /etc/kura       /var/log/kura
//	macOS         /Library/Application Support/Kura (données)      /Library/Logs/Kura
//
// KURA_DATA_DIR remplace tout (tests, développement) : configuration et journaux
// vont alors sous ce dossier, comme sous Windows.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

type layout struct{ data, config, logs string }

func current() layout {
	return layoutFor(runtime.GOOS, os.Getenv("KURA_DATA_DIR"), os.Getenv("ProgramData"))
}

func layoutFor(goos, override, programData string) layout {
	if override != "" {
		return layout{override, override, filepath.Join(override, "logs")}
	}
	switch goos {
	case "windows":
		if programData == "" {
			programData = `C:\ProgramData`
		}
		d := filepath.Join(programData, "Kura")
		return layout{d, d, filepath.Join(d, "logs")}
	case "darwin":
		d := "/Library/Application Support/Kura"
		return layout{d, d, "/Library/Logs/Kura"}
	default:
		return layout{"/var/lib/kura", "/etc/kura", "/var/log/kura"}
	}
}

// DataDir : état, coffre, bilans en attente.
func DataDir() string { return current().data }

// ConfigFile est le fichier de configuration par défaut.
func ConfigFile() string { return filepath.Join(current().config, "config.yaml") }

// StatusFile est l'état courant, relu par les icônes.
func StatusFile() string { return filepath.Join(DataDir(), "status.json") }

// HistoryFile garde une ligne JSON par exécution.
func HistoryFile() string { return filepath.Join(DataDir(), "history.jsonl") }

// RunsDir contient un dossier de journaux par exécution.
func RunsDir() string { return filepath.Join(current().logs, "runs") }

// CredentialsDir contient le coffre de secrets (chiffrés par DPAPI sous Windows).
func CredentialsDir() string { return filepath.Join(DataDir(), "credentials") }

// PendingDir garde les bilans webhook non envoyés, renvoyés à l'exécution suivante.
func PendingDir() string { return filepath.Join(DataDir(), "pending") }

// SuspendFile est la suspension de la planification (kura-agent suspend).
func SuspendFile() string { return filepath.Join(DataDir(), "suspend.json") }
