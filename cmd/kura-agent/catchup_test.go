package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/state"
)

func TestCatchUpNeeded(t *testing.T) {
	loc := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = loc })
	dir := t.TempDir()
	t.Setenv("KURA_DATA_DIR", dir)
	src, err := os.ReadFile(filepath.Join("..", "..", "configs", "example-linux.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	write := func(text string) {
		if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(string(src)) // quotidienne à 22:30, run_if_missed
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	installed := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	status := func(started time.Time, dry bool) {
		if err := state.WriteAtomic(paths.StatusFile(), state.Status{State: "idle", StartedAt: started, DryRun: dry}); err != nil {
			t.Fatal(err)
		}
	}

	// Jamais lancé depuis l'installation : le passage de lundi 22:30 est manqué.
	if ok, why := catchUpNeeded(cfg, installed, now); !ok || !strings.Contains(why, "26/10/2026 22:30") {
		t.Fatalf("%v %s", ok, why)
	}
	// Passage fait : rien.
	status(time.Date(2026, 10, 26, 22, 30, 2, 0, time.UTC), false)
	if ok, why := catchUpNeeded(cfg, installed, now); ok {
		t.Fatal(why)
	}
	// Une simulation manuelle ne compte pas comme le passage.
	status(time.Date(2026, 10, 26, 23, 0, 0, 0, time.UTC), true)
	if ok, why := catchUpNeeded(cfg, installed, now); !ok {
		t.Fatal(why)
	}
	// Installé après le dernier passage prévu : rien à rattraper.
	if ok, why := catchUpNeeded(cfg, time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC), now); ok {
		t.Fatal(why)
	}
	// run_if_missed désactivé.
	write(strings.Replace(string(src), "run_if_missed: true", "run_if_missed: false", 1))
	if ok, why := catchUpNeeded(cfg, installed, now); ok || !strings.Contains(why, "désactivé") {
		t.Fatal(why)
	}
	// Configuration illisible : lancement, pour que l'erreur soit enregistrée.
	write("version: [")
	if ok, _ := catchUpNeeded(cfg, installed, now); !ok {
		t.Fatal("configuration illisible ignorée")
	}
}
