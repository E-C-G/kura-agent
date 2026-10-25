//go:build !windows

package execx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Un faux aws en shell vérifie le vrai lancement de processus : arguments,
// environnement, sortie fusionnée et code de sortie.
func TestSystemRunPassesArgsEnvAndExitCode(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "aws")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"args=$*\"\necho \"profile=$AWS_PROFILE\" >&2\nexit 2\n"), 0o755)
	var out strings.Builder
	code, err := System{}.Run(context.Background(), script, []string{"s3", "sync", "a b"}, []string{"AWS_PROFILE=wasabi"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Errorf("code 2 attendu, reçu %d", code)
	}
	if !strings.Contains(out.String(), "args=s3 sync a b") || !strings.Contains(out.String(), "profile=wasabi") {
		t.Errorf("sortie inattendue :\n%s", out.String())
	}
}

func TestSystemRunIsCancellable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := System{}.Run(ctx, "/bin/sh", []string{"-c", "sleep 30"}, nil, &strings.Builder{})
	if err == nil {
		t.Fatal("une commande interrompue doit renvoyer une erreur")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("l'annulation doit arrêter le processus rapidement")
	}
}

func TestSystemRunMissingBinary(t *testing.T) {
	if _, err := (System{}).Run(context.Background(), "/nexiste/pas/aws", nil, nil, &strings.Builder{}); err == nil {
		t.Fatal("un binaire absent doit être une erreur de lancement")
	}
}
