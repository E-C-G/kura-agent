package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/E-C-G/kura-agent/internal/platform"
	"github.com/E-C-G/kura-agent/internal/runner"
)

// commandNeeds : fonctions dépendantes du système dont chaque commande a besoin.
// status, version et help n'en ont besoin d'aucune.
var commandNeeds = map[string][]platform.Feature{
	"run":         {platform.Vault, platform.AppControl},
	"validate":    {platform.Vault},
	"credentials": {platform.Vault},
	"import":      {platform.Vault},
	"install":     {platform.Install, platform.Scheduling, platform.Vault, platform.AWSCLISetup, platform.Tray},
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
	fmt.Fprintf(stderr, "kura-agent %s : non pris en charge sous %s dans cette version (manque : %s).\n"+
		"Windows uniquement pour l'instant ; seules status et version fonctionnent ici.\n",
		cmd, platform.OSName(), strings.Join(names, ", "))
	return false
}
