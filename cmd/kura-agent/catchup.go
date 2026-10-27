package main

import (
	"fmt"
	"time"

	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/schedtask"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// catchUpDelay : attente avant un rattrapage au démarrage (réseau, session).
var catchUpDelay = 90 * time.Second

// catchUpNeeded décide du rattrapage au démarrage (run --scheduled --catch-up) : un
// passage prévu au plus tard maintenant, postérieur au dernier lancement réel (une
// simulation ne compte pas) et à la mise en place de la planification (installed).
func catchUpNeeded(cfgPath string, installed, now time.Time) (bool, string) {
	cfg, err := config.Load(cfgPath, config.LoadOptions{})
	if err != nil {
		// La sauvegarde elle-même signalera l'erreur de configuration.
		return true, "rattrapage : configuration illisible, lancement pour signaler l'erreur"
	}
	if !cfg.Schedule.RunIfMissed {
		return false, "rattrapage : désactivé (schedule.run_if_missed)"
	}
	var last time.Time
	if st, err := state.Read(paths.StatusFile()); err == nil && st != nil && !st.DryRun {
		last = st.StartedAt
	}
	prev := schedtask.Prev(cfg.Schedule, now)
	if !schedtask.Missed(cfg.Schedule, last, installed, now) {
		return false, "rattrapage : aucun passage manqué"
	}
	return true, fmt.Sprintf("rattrapage : passage du %s manqué, sauvegarde dans %s", prev.Local().Format("02/01/2006 15:04"), catchUpDelay)
}
