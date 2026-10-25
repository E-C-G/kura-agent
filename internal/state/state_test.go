package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorst(t *testing.T) {
	if got := Worst(ResultOK, ResultWarning, ResultError, ResultSkipped); got != ResultError {
		t.Errorf("Worst = %s", got)
	}
	if got := Worst(ResultError, ResultGuard); got != ResultGuard {
		t.Errorf("le garde-fou prime sur l'erreur : %s", got)
	}
	if got := Worst(); got != ResultOK {
		t.Errorf("Worst() = %s", got)
	}
}

func TestWriteAtomicAndRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sous", "status.json")
	now := time.Now().Truncate(time.Second)
	if err := WriteAtomic(p, Status{State: "running", RunID: "r1", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(p, Status{State: "idle", RunID: "r1", StartedAt: now, Result: ResultOK}); err != nil {
		t.Fatal(err)
	}
	s, err := Read(p)
	if err != nil || s.State != "idle" || s.Result != ResultOK || !s.StartedAt.Equal(now) {
		t.Fatalf("relecture : %+v (%v)", s, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("aucun fichier temporaire ne doit rester : %v", entries)
	}
	if s, err := Read(filepath.Join(t.TempDir(), "absent.json")); s != nil || err != nil {
		t.Errorf("fichier absent : (nil, nil) attendu, reçu (%v, %v)", s, err)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "history.jsonl")
	for i := 0; i < HistoryLimit+20; i++ {
		if err := AppendHistory(p, Status{RunID: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(p)
	if n := strings.Count(string(data), "\n"); n != HistoryLimit {
		t.Errorf("%d lignes attendues, reçu %d", HistoryLimit, n)
	}
}
