package main

import (
	"runtime"
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/internal/platform"
	"github.com/E-C-G/kura-agent/pkg/config"
)

func TestCheckMissing(t *testing.T) {
	var out strings.Builder
	if !checkMissing("install", nil, false, &out) || out.Len() != 0 {
		t.Fatalf("rien ne manque : %q", out.String())
	}
	missing := []platform.Feature{platform.Install, platform.Vault}
	if checkMissing("install", missing, false, &out) {
		t.Fatal("commande acceptée sans ses fonctions")
	}
	if s := out.String(); !strings.Contains(s, "kura-agent install : non pris en charge") || !strings.Contains(s, "installation, coffre des secrets") {
		t.Fatalf("message : %q", s)
	}
	out.Reset()
	if !checkMissing("run", missing, true, &out) || !strings.Contains(out.String(), "KURA_DEV=1") {
		t.Fatalf("mode développement : %q", out.String())
	}
}

// Une commande passe si et seulement si toutes ses fonctions existent sur ce système ;
// sous Windows, toutes passent.
func TestCommandsOnThisSystem(t *testing.T) {
	t.Setenv("KURA_DEV", "")
	for cmd, needs := range commandNeeds {
		var out strings.Builder
		want := len(platform.Missing(needs...)) == 0
		if got := checkPlatform(cmd, &out); got != want || (runtime.GOOS == "windows" && !got) {
			t.Errorf("%s accepté = %v sous %s (%s)", cmd, got, runtime.GOOS, out.String())
		}
	}
	for _, cmd := range []string{"status", "version"} {
		if len(commandNeeds[cmd]) != 0 {
			t.Errorf("%s ne doit dépendre d'aucune fonction système", cmd)
		}
	}
}

func TestConfigMissing(t *testing.T) {
	cfg := &config.Config{PreTasks: []config.PreTask{{Type: "close_process", Process: "outlook.exe"}},
		PostTasks: []config.PostTask{{Type: "start_process", Process: "outlook.exe"}}}
	none := func(platform.Feature) bool { return false }
	err := configMissing(cfg, none, false)
	if err == nil || !strings.Contains(err.Error(), "pre_tasks") || !strings.Contains(err.Error(), "post_tasks") {
		t.Fatalf("%v", err)
	}
	if err := configMissing(cfg, none, true); err != nil {
		t.Fatalf("mode développement : %v", err)
	}
	if err := configMissing(&config.Config{}, none, false); err != nil {
		t.Fatalf("sans pré-tâches : %v", err)
	}
	if err := configMissing(cfg, func(platform.Feature) bool { return true }, false); err != nil {
		t.Fatalf("tout pris en charge : %v", err)
	}
	db := &config.Config{Prepare: []config.Prepare{
		{ID: "compta-db", Type: config.PrepareDBExport, Engine: config.EnginePostgreSQL},
		{ID: "gestion-db", Type: config.PrepareDBExport, Engine: config.EngineSQLServer},
	}}
	err = configMissing(db, none, false)
	if err == nil || !strings.Contains(err.Error(), "prepare[1] (gestion-db)") || strings.Contains(err.Error(), "compta-db") {
		t.Fatalf("export SQL Server hors Windows : %v", err)
	}
	dv := &config.Config{Prepare: []config.Prepare{{ID: "appli", Type: config.PrepareDocker}}}
	if err := configMissing(dv, none, false); err == nil || !strings.Contains(err.Error(), "Docker Engine sous Linux") {
		t.Fatalf("docker_volume hors Linux : %v", err)
	}
}
