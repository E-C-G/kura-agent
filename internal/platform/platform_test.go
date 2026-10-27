package platform

import (
	"errors"
	"strings"
	"testing"
)

func TestRequire(t *testing.T) {
	none := func(Feature) bool { return false }
	all := func(Feature) bool { return true }
	if err := require(all, false, All...); err != nil {
		t.Fatal(err)
	}
	err := require(func(f Feature) bool { return f != Vault }, false, Scheduling, Vault, Tray)
	var ni *NotImplementedError
	if !errors.As(err, &ni) || ni.Feature != Vault || !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(err.Error(), "coffre des secrets") || !strings.Contains(err.Error(), "Windows uniquement") {
		t.Fatalf("message : %s", err)
	}
	if err := require(none, true, All...); err != nil {
		t.Fatalf("mode développement : %v", err)
	}
	if err := require(none, false); err != nil {
		t.Fatalf("aucune fonction demandée : %v", err)
	}
}

func TestLabelsAndNames(t *testing.T) {
	for _, f := range All {
		if strings.HasPrefix(f.String(), "fonction ") {
			t.Errorf("libellé manquant pour %d", int(f))
		}
	}
	for goos, want := range map[string]string{"windows": "Windows", "linux": "Linux", "darwin": "macOS", "plan9": "plan9"} {
		if got := osName(goos); got != want {
			t.Errorf("%s → %s", goos, got)
		}
	}
}
