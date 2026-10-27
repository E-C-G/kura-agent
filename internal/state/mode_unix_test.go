//go:build !windows

package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// État et historique lisibles par tous : l'icône et kura-agent status tournent sous
// le compte de l'utilisateur, le service sous kura-agent (Linux).
func TestFilesReadableByAll(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "status.json")
	if err := WriteAtomic(st, Status{RunID: "x", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	hist := filepath.Join(dir, "history.jsonl")
	if err := AppendHistory(hist, Status{RunID: "x", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{st, hist} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("%s : %v %v", p, fi.Mode(), err)
		}
	}
}
