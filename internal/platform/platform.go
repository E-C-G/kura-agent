// Package platform dit ce que cette version de kura-agent sait faire sur le système
// où elle tourne.
//
// Le cœur de l'agent (configuration, garde-fou, synchronisation, bilan, paquet .kura,
// état) est commun à tous les systèmes. Les fonctions qui dépendent du système sont
// séparées par paquet, un fichier par système (`*_windows.go`, `*_other.go`) :
//
//	Fonction                      Paquets                         Windows   Linux, macOS
//	planification                 schedtask, systemd              oui       Linux (systemd) ; macOS à venir
//	coffre des secrets            creds                           oui       Linux (systemd-creds) ; macOS à venir
//	installation                  setup, cmd/kura-agent install   oui       non (à venir)
//	CLI AWS automatique           awsinstall                      oui       non (à venir)
//	icône et canal vers l'icône   tray, ipc                       oui       non (à venir)
//	fermeture d'applications      procs (pré-tâches)              oui       non (à venir)
//
// Hors Windows, une fonction non encore écrite renvoie une NotImplementedError, et les
// commandes qui en ont besoin s'arrêtent dès le départ avec un message clair. Les
// remplaçants de développement (secrets non chiffrés, processus lus dans /proc)
// restent utilisables avec KURA_DEV=1, jamais en production.
package platform

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

// Feature est une fonction de l'agent qui dépend du système.
type Feature int

// Fonctions dépendantes du système.
const (
	Scheduling  Feature = iota + 1 // tâche planifiée (Windows : \Kura\Sync)
	Vault                          // coffre des secrets (Windows : DPAPI)
	Install                        // installation, désinstallation
	AWSCLISetup                    // installation automatique de la CLI AWS
	Tray                           // icône par session et canal vers elle
	AppControl                     // fermeture et relance d'applications (pré-tâches)
)

// All : toutes les fonctions, dans l'ordre d'affichage.
var All = []Feature{Scheduling, Vault, Install, AWSCLISetup, Tray, AppControl}

var labels = map[Feature]string{
	Scheduling:  "planification",
	Vault:       "coffre des secrets",
	Install:     "installation",
	AWSCLISetup: "installation de la CLI AWS",
	Tray:        "icône",
	AppControl:  "fermeture d'applications",
}

func (f Feature) String() string {
	if l, ok := labels[f]; ok {
		return l
	}
	return fmt.Sprintf("fonction %d", int(f))
}

// ErrNotImplemented : fonction non encore écrite pour ce système.
var ErrNotImplemented = errors.New("non pris en charge sur ce système dans cette version")

// NotImplementedError précise la fonction et le système.
type NotImplementedError struct {
	Feature Feature
	OS      string
}

func (e *NotImplementedError) Error() string {
	return fmt.Sprintf("%s : non pris en charge sous %s dans cette version (Windows uniquement pour l'instant)", e.Feature, e.OS)
}

// Unwrap : errors.Is(err, ErrNotImplemented).
func (e *NotImplementedError) Unwrap() error { return ErrNotImplemented }

// NotImplemented renvoie l'erreur d'une fonction absente sur ce système.
func NotImplemented(f Feature) error { return &NotImplementedError{Feature: f, OS: OSName()} }

// OSName : nom du système pour les messages.
func OSName() string { return osName(runtime.GOOS) }

func osName(goos string) string {
	switch goos {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	}
	return goos
}

// Supported indique si la fonction est écrite pour ce système.
func Supported(f Feature) bool { return implemented[f] }

// DevMode : KURA_DEV=1 autorise, hors Windows, les remplaçants de développement
// (secrets non chiffrés, processus lus dans /proc). Jamais en production.
func DevMode() bool { return os.Getenv("KURA_DEV") == "1" }

// Require renvoie l'erreur de la première fonction absente, sauf en mode
// développement.
func Require(fs ...Feature) error { return require(Supported, DevMode(), fs...) }

// Missing liste les fonctions absentes parmi fs, mode développement ignoré.
func Missing(fs ...Feature) []Feature {
	var out []Feature
	for _, f := range fs {
		if !Supported(f) {
			out = append(out, f)
		}
	}
	return out
}

func require(supported func(Feature) bool, dev bool, fs ...Feature) error {
	if dev {
		return nil
	}
	for _, f := range fs {
		if !supported(f) {
			return NotImplemented(f)
		}
	}
	return nil
}
