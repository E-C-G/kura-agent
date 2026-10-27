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
