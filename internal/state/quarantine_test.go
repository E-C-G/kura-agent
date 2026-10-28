package state

import (
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
