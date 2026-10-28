package main

import (
	"context"
	"runtime"
	"time"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/prepare"
	"github.com/E-C-G/kura-agent/internal/setup"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// toolCheck : vérification d'un outil des préparations (tools).
type toolCheck struct {
	Field, Path, Version string
	Err                  error
}

// checkPrepareTools vérifie les outils dont les préparations déclarées ont besoin :
// chemin absolu, droits (modifiable par les administrateurs seuls), version reconnue.
// Remplaçable en test.
var checkPrepareTools = func(cfg *config.Config) []toolCheck {
	var out []toolCheck
	for _, p := range cfg.Prepare {
		if p.Type == config.PrepareZip {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			v, err := prepare.CheckZip(ctx, execx.System{}, cfg.Tools.Zip)
			cancel()
			out = append(out, toolCheck{Field: "tools.zip", Path: cfg.Tools.Zip, Version: v, Err: err})
			break
		}
	}
	return out
}

// agentDirs : dossiers de l'agent, jamais archivés par une préparation ni servis depuis
// l'icône.
func agentDirs() []string {
	dirs := []string{paths.DataDir(), paths.ConfigDir(), paths.LogsDir(), paths.CredentialsDir(), setup.ProgramDir()}
	if runtime.GOOS == "linux" {
		// Secrets déchiffrés par systemd pour le service, et clé de l'hôte qui les chiffre.
		dirs = append(dirs, "/run/credentials", "/var/lib/systemd/credential.secret")
	}
	return dirs
}
