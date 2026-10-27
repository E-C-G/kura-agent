package main

import (
	"runtime"
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/internal/platform"
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

// Sous Windows, toutes les commandes passent ; ailleurs, seules status et version.
func TestCommandsOnThisSystem(t *testing.T) {
	t.Setenv("KURA_DEV", "")
	for cmd := range commandNeeds {
		var out strings.Builder
		if got, want := checkPlatform(cmd, &out), runtime.GOOS == "windows"; got != want {
			t.Errorf("%s accepté = %v sous %s (%s)", cmd, got, runtime.GOOS, out.String())
		}
	}
	for _, cmd := range []string{"status", "version"} {
		if len(commandNeeds[cmd]) != 0 {
			t.Errorf("%s ne doit dépendre d'aucune fonction système", cmd)
		}
	}
}
