package report

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/pkg/config"
)

func restoreInfo() RestoreInfo {
	at := time.Date(2026, 10, 20, 23, 59, 59, 0, time.UTC)
	return RestoreInfo{Account: "ACME\\admin", Paths: []string{`D:\Projets\Clients`}, To: `E:\Restauration`, At: &at,
		Files: 37, Bytes: 441344, Failed: 1, Result: "partial", ExitCode: 1,
		Started: time.Unix(1_800_000_000, 0), Ended: time.Unix(1_800_000_100, 0)}
}

// healthchecks : /log seulement, l'état du check (sauvegarde) ne change pas.
func TestRestoreHealthchecksLog(t *testing.T) {
	s := newServer(t, nil)
	err := Restore(context.Background(), &config.Report{Type: config.ReportHealthchecks, URL: s.URL + "/ping/abc"}, creds.Store{}, opts(s, ""), restoreInfo())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.hits) != 1 || s.hits[0].path != "/ping/abc/log" {
		t.Fatalf("%+v", s.hits)
	}
	if b := s.hits[0].body; !strings.Contains(b, "restore par ACME\\admin : partial") || !strings.Contains(b, "37 fichier(s)") || !strings.Contains(b, "1 en échec") {
		t.Errorf("%s", b)
	}
}

func TestRestoreWebhook(t *testing.T) {
	s := newServer(t, nil)
	store := creds.Store{Dir: t.TempDir()}
	store.Set("webhook", "jeton-42")
	pending := t.TempDir()
	err := Restore(context.Background(), &config.Report{Type: config.ReportWebhook, URL: s.URL + "/kura", Token: "credential:webhook", HMAC: true},
		store, opts(s, pending), restoreInfo())
	if err != nil {
		t.Fatal(err)
	}
	h := s.hits[0]
	if want := "sha256=" + Sign("jeton-42", h.header.Get("X-Kura-Timestamp"), []byte(h.body)); h.header.Get("X-Kura-Signature") != want {
		t.Error("signature")
	}
	var p RestorePayload
	if err := json.Unmarshal([]byte(h.body), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "restore" || p.Event != "restore_finished" || p.Host != "SRV01" || p.Files != 37 || p.Account != "ACME\\admin" ||
		p.At == nil || len(p.Paths) != 1 || p.SentAt.IsZero() {
		t.Errorf("%+v", p)
	}

	// pas de file d'attente : un bilan de restauration non remis est seulement signalé
	down := newServer(t, func(int) int { return 503 })
	if err := Restore(context.Background(), &config.Report{Type: config.ReportWebhook, URL: down.URL}, creds.Store{}, opts(down, pending), restoreInfo()); err == nil {
		t.Fatal("erreur attendue")
	}
	if files := (&webhook{opt: Options{PendingDir: pending}}).pendingFiles(); len(files) != 0 {
		t.Fatalf("mis en attente : %v", files)
	}
	if err := Restore(context.Background(), nil, creds.Store{}, opts(s, ""), restoreInfo()); err != nil {
		t.Fatal("sans destination :", err)
	}
}
