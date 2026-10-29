package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/docker"

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
		case p.Type == config.PrepareGPG:
			field, path = "tools.gpg", cfg.Tools.GPG
			check = func(ctx context.Context) (string, error) { return prepare.CheckGPG(ctx, execx.System{}, path) }
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

// keyCheck : clés gpg d'une préparation, vérifiées par validate.
type keyCheck struct {
	ID           string
	Fingerprints []string
	Warnings     []string
	Err          error
}

// checkGPGKeys monte le trousseau de chaque préparation gpg dans un dossier temporaire
// et vérifie ses clés (empreinte, clé publique seule, ni révoquée ni expirée).
// Remplaçable en test.
var checkGPGKeys = func(cfg *config.Config) []keyCheck {
	var out []keyCheck
	for _, p := range cfg.Prepare {
		if p.Type != config.PrepareGPG {
			continue
		}
		dir, err := os.MkdirTemp("", "kura-validate-")
		if err != nil {
			out = append(out, keyCheck{ID: p.ID, Err: err})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		fprs, warns, err := prepare.CheckGPGKeys(ctx, execx.System{}, cfg, p, dir)
		cancel()
		os.RemoveAll(dir)
		out = append(out, keyCheck{ID: p.ID, Fingerprints: fprs, Warnings: warns, Err: err})
	}
	return out
}

// dockerCheck : volumes d'une préparation docker_volume, vérifiés par validate.
type dockerCheck struct {
	ID, Detail string
	Err        error
}

// checkDockerVolumes vérifie, pour chaque préparation docker_volume, le socket et l'API
// (gel), puis la présence de chaque volume et les conteneurs qui le montent ; sans gel,
// la présence des dossiers des volumes. Remplaçable en test.
var checkDockerVolumes = func(cfg *config.Config) []dockerCheck {
	var out []dockerCheck
	for _, p := range cfg.Prepare {
		if p.Type != config.PrepareDocker {
			continue
		}
		out = append(out, checkDocker(p))
	}
	return out
}

func checkDocker(p config.Prepare) dockerCheck {
	c := dockerCheck{ID: p.ID}
	if !p.Paused() {
		for _, v := range p.Volumes {
			dir := filepath.Join(p.DataRoot, "volumes", v, "_data")
			if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
				c.Err = fmt.Errorf("volume %s : %s absent ou n'est pas un dossier", v, dir)
				return c
			}
		}
		c.Detail = "pause: false : copie non figée, sans accès à Docker"
		return c
	}
	if err := docker.CheckSocket(p.Socket); err != nil {
		c.Err = err
		return c
	}
	api := docker.New(p.Socket)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	version, err := api.Negotiate(ctx)
	if err != nil {
		c.Err = err
		return c
	}
	for _, v := range p.Volumes {
		vol, err := api.Volume(ctx, v)
		if err != nil {
			c.Err = fmt.Errorf("volume %s : %w", v, err)
			return c
		}
		if vol.Driver != "local" {
			c.Err = fmt.Errorf("volume %s : pilote %q, seul local est pris en charge", v, vol.Driver)
			return c
		}
	}
	ks, err := api.Using(ctx, p.Volumes)
	if err != nil {
		c.Err = err
		return c
	}
	var names []string
	for _, k := range ks {
		if k.State == "running" {
			names = append(names, k.Name())
		}
	}
	gel := "aucun conteneur en marche"
	if len(names) > 0 {
		gel = "gelés pendant la lecture : " + strings.Join(names, ", ")
	}
	c.Detail = fmt.Sprintf("Docker %s · %s · le service rejoint le groupe docker (équivaut aux droits root)", version, gel)
	return c
}

// needsDockerGroup : une préparation docker_volume gèle les conteneurs (socket Docker).
func needsDockerGroup(cfg *config.Config) bool {
	for _, p := range cfg.Prepare {
		if p.Type == config.PrepareDocker && p.Paused() {
			return true
		}
	}
	return false
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
