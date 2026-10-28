package main

import (
	"context"
	"os"
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
	done := map[string]bool{}
	for _, p := range cfg.Prepare {
		var field, path string
		var check func(ctx context.Context) (string, error)
		switch {
		case p.Type == config.PrepareZip:
			field, path = "tools.zip", cfg.Tools.Zip
			check = func(ctx context.Context) (string, error) { return prepare.CheckZip(ctx, execx.System{}, path) }
		case p.Type == config.PrepareDBExport:
			field, path = dbToolField(p.Engine, cfg)
			check = func(ctx context.Context) (string, error) { return prepare.CheckDBTools(ctx, execx.System{}, cfg, p) }
		default:
			continue
		}
		if done[field] {
			continue
		}
		done[field] = true
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		v, err := check(ctx)
		cancel()
		out = append(out, toolCheck{Field: field, Path: path, Version: v, Err: err})
	}
	return out
}

// dbToolField : champ et chemin de l'outil d'export d'un moteur.
func dbToolField(engine string, cfg *config.Config) (string, string) {
	switch engine {
	case config.EnginePostgreSQL:
		return "tools.pg_dump", cfg.Tools.PgDump
	case config.EngineMySQL:
		return "tools.mysqldump", cfg.Tools.MySQLDump
	}
	return "tools.sqlcmd", cfg.Tools.SQLCmd
}

// dbCheck : connexion d'une préparation db_export essayée par validate.
type dbCheck struct {
	ID, Note  string
	Databases []string
	Err       error
}

// checkDBConnections se connecte à chaque base des préparations db_export, avec le
// rôle et le mot de passe du coffre, et liste les bases exportées. Une connexion sans
// utilisateur (authentification du système, SQL Server par le compte SYSTEM) n'est pas
// essayée ici : validate tourne sous le compte de l'administrateur, pas celui de la
// tâche. Remplaçable en test.
var checkDBConnections = func(cfg *config.Config) []dbCheck {
	var out []dbCheck
	for _, p := range cfg.Prepare {
		if p.Type != config.PrepareDBExport {
			continue
		}
		if p.User == "" {
			out = append(out, dbCheck{ID: p.ID, Note: "authentification du système : vérifiée à la première exécution, sous le compte de la tâche"})
			continue
		}
		dir, err := os.MkdirTemp("", "kura-validate-")
		if err != nil {
			out = append(out, dbCheck{ID: p.ID, Err: err})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		dbs, err := prepare.CheckDBConnection(ctx, execx.System{}, cfg, p, dir, vault().Resolve)
		cancel()
		os.RemoveAll(dir)
		out = append(out, dbCheck{ID: p.ID, Databases: dbs, Err: err})
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
