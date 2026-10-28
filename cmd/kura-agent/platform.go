package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/E-C-G/kura-agent/pkg/config"

	"github.com/E-C-G/kura-agent/internal/platform"
	"github.com/E-C-G/kura-agent/internal/runner"
)

// commandNeeds : fonctions dépendantes du système dont chaque commande a besoin.
// status, version et help n'en ont besoin d'aucune.
var commandNeeds = map[string][]platform.Feature{
	"run":         {platform.Vault}, // pré- et post-tâches : platformConfig
	"validate":    {platform.Vault},
	"credentials": {platform.Vault},
	"import":      {platform.Vault},
	"restore":     {platform.Vault},
	"install":     {platform.Install, platform.Scheduling, platform.Vault, platform.AWSCLISetup}, // icône : si elle existe
	"uninstall":   {platform.Install, platform.Scheduling},
	"suspend":     {platform.Scheduling},
	"tray":        {platform.Tray},
}

// unsupportedCode : code de sortie d'une commande refusée sur ce système.
func unsupportedCode(cmd string) int {
	switch cmd {
	case "run", "validate":
		return runner.ExitConfig
	}
	return 2
}

// checkPlatform vérifie que la commande peut tourner sur ce système. Hors Windows,
// elle est refusée avec la liste de ce qui manque, sauf KURA_DEV=1 (remplaçants de
// développement, signalés).
func checkPlatform(cmd string, stderr io.Writer) bool {
	return checkMissing(cmd, platform.Missing(commandNeeds[cmd]...), platform.DevMode(), stderr)
}

func checkMissing(cmd string, missing []platform.Feature, dev bool, stderr io.Writer) bool {
	if len(missing) == 0 {
		return true
	}
	names := make([]string, len(missing))
	for i, f := range missing {
		names[i] = f.String()
	}
	if dev {
		fmt.Fprintf(stderr, "kura-agent %s : mode développement (KURA_DEV=1) sous %s, remplaçants non sûrs pour : %s. Jamais en production.\n",
			cmd, platform.OSName(), strings.Join(names, ", "))
		return true
	}
	fmt.Fprintf(stderr, "kura-agent %s : non pris en charge sous %s dans cette version (manque : %s).\n",
		cmd, platform.OSName(), strings.Join(names, ", "))
	return false
}

// platformConfig refuse une configuration qui demande une fonction absente de ce
// système : pré-tâches (fermeture d'applications) et post-tâches (relance par
// l'icône). Sans elles, la sauvegarde tourne.
func platformConfig(cfg *config.Config) error {
	return configMissing(cfg, platform.Supported, platform.DevMode())
}

func configMissing(cfg *config.Config, supported func(platform.Feature) bool, dev bool) error {
	if dev {
		return nil
	}
	var errs []error
	if len(cfg.PreTasks) > 0 && !supported(platform.AppControl) {
		errs = append(errs, fmt.Errorf("pre_tasks : %w", platform.NotImplemented(platform.AppControl)))
	}
	if len(cfg.PostTasks) > 0 && !supported(platform.Tray) {
		errs = append(errs, fmt.Errorf("post_tasks : %w", platform.NotImplemented(platform.Tray)))
	}
	for i, p := range cfg.Prepare {
		if p.Type == config.PrepareDBExport && p.Engine == config.EngineSQLServer && !supported(platform.SQLServerExport) {
			errs = append(errs, fmt.Errorf("prepare[%d] (%s) : %w", i, p.ID, platform.NotImplemented(platform.SQLServerExport)))
		}
	}
	return errors.Join(errs...)
}
