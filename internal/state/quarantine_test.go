package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuarantine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "quarantine.json")
	if q, err := ReadQuarantine(p); err != nil || len(q) != 0 {
		t.Fatal(q, err)
	}
	now := time.Now()
	AddQuarantine(p, []string{"C/alice/Outlook/j/", "C/alice/Outlook/j"}, now)
	AddQuarantine(p, []string{"D/x"}, now)
	q, err := ReadQuarantine(p)
	if err != nil || len(q) != 2 {
		t.Fatal(q, err)
	}
	for key, want := range map[string]bool{"C/alice/Outlook/j": true, "C/alice/Outlook/j/wasabi.secret": true,
		"C/alice/Outlook/jx": false, "D/x/y": true, "D": false} {
		if Quarantined(q, key) != want {
			t.Errorf("%s : %v", key, !want)
		}
	}
}

// Un fichier abîmé n'est jamais écrasé : ses entrées seraient perdues.
func TestQuarantineCorruptIsKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "quarantine.json")
	os.WriteFile(p, []byte("{abîmé"), 0o644)
	if _, err := ReadQuarantine(p); err == nil {
		t.Fatal("lu sans erreur")
	}
	if err := AddQuarantine(p, []string{"x"}, time.Now()); err == nil {
		t.Fatal("ajout sans erreur")
	}
	if b, _ := os.ReadFile(p); string(b) != "{abîmé" {
		t.Fatal("fichier réécrit")
	}
}
