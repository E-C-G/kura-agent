package report

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

type hit struct {
	path, rid, body string
	header          http.Header
}

// server enregistre les requêtes ; status choisit le code de réponse de la n-ième requête.
type server struct {
	mu     sync.Mutex
	hits   []hit
	status func(n int) int
	*httptest.Server
}

func newServer(t *testing.T, status func(n int) int) *server {
	s := &server{status: status}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		n := len(s.hits)
		s.hits = append(s.hits, hit{path: r.URL.Path, rid: r.URL.Query().Get("rid"), body: string(body), header: r.Header.Clone()})
		s.mu.Unlock()
		code := http.StatusOK
		if s.status != nil {
			code = s.status(n)
		}
		w.WriteHeader(code)
		io.WriteString(w, "OK")
	}))
	t.Cleanup(s.Close)
	return s
}

func opts(s *server, dir string) Options {
	return Options{
		Version: "1.2.3", Host: "SRV01", Profile: "acme-srv01", Bucket: "acme-srv01",
		PendingDir: dir, Client: s.Client(), Backoff: []time.Duration{0, 0},
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		Now:   func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
}

func status(result string, success bool) state.Status {
	start := time.Unix(1_799_990_000, 0)
	end := time.Unix(1_799_999_000, 0)
	st := state.Status{RunID: "2027-01-15_0800", Result: result, StartedAt: start, EndedAt: &end,
		Jobs: []state.JobStatus{{ID: "data", Result: result, Uploaded: 3, Deleted: 1}}}
	if success {
		st.LastSuccess = &end
	}
	return st
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestHealthchecksStartSuccessFail(t *testing.T) {
	s := newServer(t, nil)
	r, err := New(&config.Report{Type: config.ReportHealthchecks, URL: s.URL + "/ping/abc/"}, creds.Store{}, opts(s, ""))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r.Start(ctx, state.Status{}, true)
	r.Finish(ctx, status(state.ResultOK, true), true)
	if len(s.hits) != 2 {
		t.Fatalf("2 requêtes attendues, reçu %+v", s.hits)
	}
	if s.hits[0].path != "/ping/abc/start" || s.hits[1].path != "/ping/abc" {
		t.Errorf("chemins : %q puis %q", s.hits[0].path, s.hits[1].path)
	}
	if !uuidRe.MatchString(s.hits[0].rid) || s.hits[0].rid != s.hits[1].rid {
		t.Errorf("le même rid (UUID v4) doit relier début et fin : %q / %q", s.hits[0].rid, s.hits[1].rid)
	}
	if !strings.Contains(s.hits[1].body, "job data : ok, 3 envoi(s), 1 suppression(s)") {
		t.Errorf("le résumé doit partir dans le corps : %q", s.hits[1].body)
	}
	if ua := s.hits[1].header.Get("User-Agent"); ua != "kura-agent/1.2.3" {
		t.Errorf("User-Agent : %q", ua)
	}

	for _, tc := range []struct {
		st   state.Status
		path string
	}{
		{status(state.ResultGuard, false), "/ping/abc/fail"},
		{status(state.ResultError, false), "/ping/abc/fail"},
		{status(state.ResultWarning, false), "/ping/abc/fail"}, // job sauté : pas une sauvegarde complète
		{status(state.ResultWarning, true), "/ping/abc"},       // fichiers verrouillés : réussite
	} {
		s.hits = nil
		r.Finish(ctx, tc.st, true)
		if len(s.hits) != 1 || s.hits[0].path != tc.path {
			t.Errorf("%s (succès=%v) : %q attendu, reçu %+v", tc.st.Result, tc.st.LastSuccess != nil, tc.path, s.hits)
		}
	}
}

func TestHealthchecksIgnoresPartialRuns(t *testing.T) {
	s := newServer(t, nil)
	r, _ := New(&config.Report{Type: config.ReportHealthchecks, URL: s.URL + "/ping/abc"}, creds.Store{}, opts(s, ""))
	r.Start(context.Background(), state.Status{}, false)
	if err := r.Finish(context.Background(), status(state.ResultOK, false), false); err != nil || len(s.hits) != 0 {
		t.Fatalf("une exécution partielle (--job) ne touche pas au check : %+v, %v", s.hits, err)
	}
}

func TestRetriesOnServerErrorNotOn404(t *testing.T) {
	flaky := newServer(t, func(n int) int {
		if n < 2 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	r, _ := New(&config.Report{Type: config.ReportHealthchecks, URL: flaky.URL + "/ping/abc"}, creds.Store{}, opts(flaky, ""))
	if err := r.Finish(context.Background(), status(state.ResultOK, true), true); err != nil || len(flaky.hits) != 3 {
		t.Fatalf("503, 503 puis 200 : réussite au 3e essai attendue, %d essais, %v", len(flaky.hits), err)
	}

	missing := newServer(t, func(int) int { return http.StatusNotFound })
	r, _ = New(&config.Report{Type: config.ReportHealthchecks, URL: missing.URL + "/ping/inconnu"}, creds.Store{}, opts(missing, ""))
	err := r.Finish(context.Background(), status(state.ResultOK, true), true)
	if err == nil || len(missing.hits) != 1 {
		t.Fatalf("404 : une seule tentative et une erreur attendues, %d essais, %v", len(missing.hits), err)
	}
	if strings.Contains(err.Error(), missing.URL) {
		t.Errorf("l'URL (qui peut être secrète) ne doit pas apparaître dans l'erreur : %v", err)
	}
}

func TestWebhookPayloadTokenAndSignature(t *testing.T) {
	s := newServer(t, nil)
	store := creds.Store{Dir: t.TempDir()}
	store.Set("webhook", "jeton-42")
	r, err := New(&config.Report{Type: config.ReportWebhook, URL: s.URL + "/kura", Token: "credential:webhook", HMAC: true},
		store, opts(s, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), state.Status{}, true); err != nil || len(s.hits) != 0 {
		t.Fatal("le webhook n'envoie rien au début de l'exécution")
	}
	if err := r.Finish(context.Background(), status(state.ResultOK, true), true); err != nil {
		t.Fatal(err)
	}
	h := s.hits[0]
	if h.header.Get("X-Kura-Token") != "" || h.header.Get("Content-Type") != "application/json" {
		t.Errorf("en-têtes : %v", h.header)
	}
	ts := h.header.Get("X-Kura-Timestamp")
	if want := "sha256=" + Sign("jeton-42", ts, []byte(h.body)); h.header.Get("X-Kura-Signature") != want {
		t.Errorf("signature %q, attendu %q", h.header.Get("X-Kura-Signature"), want)
	}
	var p Payload
	if err := json.Unmarshal([]byte(h.body), &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != "run_finished" || p.Host != "SRV01" || p.Bucket != "acme-srv01" || !p.Success || p.Partial ||
		p.AgentVersion != "1.2.3" || len(p.Jobs) != 1 || p.Jobs[0].Uploaded != 3 {
		t.Errorf("bilan inattendu : %+v", p)
	}
	if strings.Contains(h.body, "jeton-42") {
		t.Error("le jeton ne doit jamais figurer dans le corps")
	}
}

func TestWebhookWithoutToken(t *testing.T) {
	s := newServer(t, nil)
	r, _ := New(&config.Report{Type: config.ReportWebhook, URL: s.URL + "/kura"}, creds.Store{}, opts(s, t.TempDir()))
	r.Finish(context.Background(), status(state.ResultError, false), false)
	var p Payload
	json.Unmarshal([]byte(s.hits[0].body), &p)
	if s.hits[0].header.Get("X-Kura-Token") != "" || p.Success || !p.Partial || p.Result != state.ResultError {
		t.Errorf("sans jeton, exécution partielle en erreur : %+v / %v", p, s.hits[0].header)
	}
}

func TestWebhookQueuesAndResendsInOrder(t *testing.T) {
	down := true
	var mu sync.Mutex
	s := newServer(t, func(int) int {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return http.StatusBadGateway
		}
		return http.StatusOK
	})
	pending := t.TempDir()
	r, _ := New(&config.Report{Type: config.ReportWebhook, URL: s.URL + "/kura"}, creds.Store{}, opts(s, pending))

	first := status(state.ResultOK, true)
	first.RunID = "2027-01-14_2230"
	if err := r.Finish(context.Background(), first, true); err == nil {
		t.Fatal("broker indisponible : une erreur est attendue")
	}
	if files, _ := filepath.Glob(filepath.Join(pending, "*.json")); len(files) != 1 {
		t.Fatalf("le bilan doit être gardé pour la prochaine fois : %v", files)
	}

	mu.Lock()
	down = false
	mu.Unlock()
	s.hits = nil
	second := status(state.ResultOK, true)
	second.RunID = "2027-01-15_2230"
	if err := r.Finish(context.Background(), second, true); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range s.hits {
		var p Payload
		json.Unmarshal([]byte(h.body), &p)
		ids = append(ids, p.RunID)
	}
	if strings.Join(ids, ",") != "2027-01-14_2230,2027-01-15_2230" {
		t.Errorf("le bilan en attente doit partir avant le nouveau : %v", ids)
	}
	if files, _ := filepath.Glob(filepath.Join(pending, "*.json")); len(files) != 0 {
		t.Errorf("la file d'attente doit être vidée : %v", files)
	}
}

func TestPendingQueueIsBounded(t *testing.T) {
	s := newServer(t, func(int) int { return http.StatusBadGateway })
	pending := t.TempDir()
	r, _ := New(&config.Report{Type: config.ReportWebhook, URL: s.URL}, creds.Store{}, opts(s, pending))
	for i := 0; i < pendingLimit+5; i++ {
		st := status(state.ResultOK, true)
		st.RunID = time.Unix(int64(i)*3600, 0).UTC().Format("2006-01-02_1504")
		r.Finish(context.Background(), st, true)
	}
	files, _ := filepath.Glob(filepath.Join(pending, "*.json"))
	if len(files) != pendingLimit {
		t.Fatalf("%d bilans en attente au plus, reçu %d", pendingLimit, len(files))
	}
	if !strings.HasSuffix(files[len(files)-1], time.Unix(int64(pendingLimit+4)*3600, 0).UTC().Format("2006-01-02_1504")+".json") {
		t.Errorf("les plus récents doivent être gardés : %v", files[len(files)-1])
	}
}

func TestMissingCredentialIsReported(t *testing.T) {
	_, err := New(&config.Report{Type: config.ReportHealthchecks, URL: "credential:hc-url"}, creds.Store{Dir: t.TempDir()}, Options{})
	if err == nil || !strings.Contains(err.Error(), "credentials set hc-url") {
		t.Fatalf("un secret absent doit être signalé avec la commande à lancer : %v", err)
	}
}

func TestCheckUsesLogEndpoint(t *testing.T) {
	s := newServer(t, nil)
	note, err := Check(context.Background(), &config.Report{Type: config.ReportHealthchecks, URL: s.URL + "/ping/abc"}, creds.Store{}, opts(s, ""))
	if err != nil || len(s.hits) != 1 || s.hits[0].path != "/ping/abc/log" || note == "" {
		t.Fatalf("validate doit utiliser /log (sans changer l'état du check) : %+v, %v", s.hits, err)
	}
	s.hits = nil
	if _, err := Check(context.Background(), &config.Report{Type: config.ReportWebhook, URL: s.URL}, creds.Store{}, opts(s, "")); err != nil || len(s.hits) != 0 {
		t.Fatalf("validate n'appelle pas le webhook : %+v, %v", s.hits, err)
	}
}

func TestNilReportMeansNoReporter(t *testing.T) {
	r, err := New(nil, creds.Store{}, Options{})
	if r != nil || err != nil {
		t.Fatalf("pas de destination : (nil, nil) attendu, reçu (%v, %v)", r, err)
	}
}

// Audit M3 : une redirection n'est jamais suivie (le jeton et l'URL de ping ne partent
// pas ailleurs), même avec le client fourni par l'appelant.
func TestRedirectNotFollowed(t *testing.T) {
	elsewhere := newServer(t, nil)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/vol", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	o := opts(elsewhere, t.TempDir())
	o.Client = redirect.Client() // même autorité de test, mais pas de CheckRedirect
	r, err := New(&config.Report{Type: config.ReportWebhook, URL: redirect.URL + "/kura", Token: "jeton-42"}, creds.Store{}, o)
	if err != nil {
		t.Fatal(err)
	}
	err = r.Finish(context.Background(), status(state.ResultOK, true), true)
	if err == nil || !strings.Contains(err.Error(), "redirection non suivie") {
		t.Fatalf("redirection : %v", err)
	}
	if len(elsewhere.hits) != 0 {
		t.Fatalf("redirection suivie : %+v", elsewhere.hits)
	}
}

func TestWebhookTokenWithoutHMAC(t *testing.T) {
	s := newServer(t, nil)
	r, err := New(&config.Report{Type: config.ReportWebhook, URL: s.URL + "/kura", Token: "jeton-42"}, creds.Store{}, opts(s, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	r.Finish(context.Background(), status(state.ResultOK, true), true)
	if h := s.hits[0].header; h.Get("X-Kura-Token") != "jeton-42" || h.Get("X-Kura-Signature") != "" {
		t.Fatalf("en-têtes : %v", h)
	}
}

// Une URL http rangée dans le coffre est refusée, comme dans la configuration.
func TestResolvedURLMustBeHTTPS(t *testing.T) {
	t.Setenv("KURA_DEV", "1")
	store := creds.Store{Dir: t.TempDir()}
	if err := store.Set("hc-url", "http://hc-ping.example/uuid"); err != nil {
		t.Fatal(err)
	}
	_, err := New(&config.Report{Type: config.ReportHealthchecks, URL: "credential:hc-url"}, store, Options{})
	if err == nil || !strings.Contains(err.Error(), "https") || strings.Contains(err.Error(), "hc-ping.example") {
		t.Fatalf("URL http du coffre : %v", err)
	}
}
