package setup

import (
	"errors"
	"os/user"
	"strings"
	"testing"
)

func TestEnsureServiceAccount(t *testing.T) {
	exists := false
	var lines []string
	runSysusers = func(l string) error { lines = append(lines, l); exists = true; return nil }
	lookupUser = func(n string) (*user.User, error) {
		if exists && n == ServiceAccount {
			return &user.User{Username: n}, nil
		}
		return nil, user.UnknownUserError(n)
	}
	t.Cleanup(func() { lookupUser = user.Lookup })
	created, err := EnsureServiceAccount()
	if err != nil || !created || len(lines) != 1 || !strings.HasPrefix(lines[0], "u kura-agent - ") || !strings.Contains(lines[0], "/var/lib/kura") {
		t.Fatalf("%v %v %q", created, err, lines)
	}
	// Déjà là : rien à faire.
	if created, err := EnsureServiceAccount(); err != nil || created || len(lines) != 1 {
		t.Fatalf("%v %v", created, err)
	}
	// Échec de systemd-sysusers : remonté.
	exists = false
	runSysusers = func(string) error { return errors.New("refus") }
	if _, err := EnsureServiceAccount(); err == nil {
		t.Fatal("échec ignoré")
	}
}
