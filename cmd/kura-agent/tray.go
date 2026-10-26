package main

import (
	"flag"

	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/tray"
)

func cmdTray(args []string) int {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	cfgPath := fs.String("config", paths.ConfigFile(), "fichier de configuration")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return tray.Main(tray.Options{ConfigFile: *cfgPath, Version: version, Commit: commit})
}
