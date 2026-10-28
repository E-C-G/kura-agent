package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/internal/selfupdate"
)

// fakeReleases sert la release v0.8.2 réelle et son attestation (données de
// internal/selfupdate/testdata).
func fakeReleases(t *testing.T) *selfupdate.Client {
	t.Helper()
	dir := filepath.Join("..", "..", "internal", "selfupdate", "testdata")
	rel, err := os.ReadFile(filepath.Join(dir, "release_v0.8.2.json"))
	if err != nil {
		t.Fatal(err)
	}
	att, err := os.ReadFile(filepath.Join(dir, "attestations_v0.8.2.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/attestations/") {
			_, _ = w.Write(att)
			return
		}
		_, _ = w.Write(rel)
	}))
	t.Cleanup(srv.Close)
	return &selfupdate.Client{API: srv.URL, HTTP: srv.Client()}
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

func TestSelfUpdateCheck(t *testing.T) {
	c := fakeReleases(t)
	var out, errb bytes.Buffer

	withVersion(t, "0.8.1")
	if code := runSelfUpdate([]string{"--check"}, &out, &errb, c); code != exitUpdateNewer ||
		!strings.Contains(out.String(), "Mise à jour disponible") || !strings.Contains(out.String(), "attestation de release GitHub vérifiée") {
		t.Fatalf("code %d\n%s%s", code, out.String(), errb.String())
	}

	out.Reset()
	withVersion(t, "0.8.2")
	if code := runSelfUpdate([]string{"--check", "--json"}, &out, &errb, c); code != exitUpdateOK {
		t.Fatalf("à jour : code %d %s", code, errb.String())
	}
	var j selfUpdateOut
	if err := json.Unmarshal(out.Bytes(), &j); err != nil || j.Newer || !j.Verified || j.Latest != "0.8.2" || j.SHA256 == "" {
		t.Fatalf("JSON : %+v %v\n%s", j, err, out.String())
	}

	out.Reset()
	withVersion(t, "dev")
	if code := runSelfUpdate([]string{"--check"}, &out, &errb, c); code != exitUpdateOK || !strings.Contains(out.String(), "développement") {
		t.Fatalf("dev : code %d\n%s", code, out.String())
	}
}

func TestSelfUpdateCheckFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "panne", http.StatusBadGateway)
	}))
	defer srv.Close()
	withVersion(t, "0.8.1")
	var out, errb bytes.Buffer
	c := &selfupdate.Client{API: srv.URL, HTTP: srv.Client()}
	if code := runSelfUpdate([]string{"--check"}, &out, &errb, c); code != exitUpdateFailed || !strings.Contains(errb.String(), "502") {
		t.Fatalf("code %d %s", code, errb.String())
	}
}

func TestUpdateProxyFromConfig(t *testing.T) {
	dir := t.TempDir()
	var errb bytes.Buffer
	if p, err := updateProxy(filepath.Join(dir, "absent.yaml"), &errb); err != nil || p != "" {
		t.Fatalf("fichier absent : %q %v", p, err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	// Le reste du fichier n'a pas à être valide.
	if err := os.WriteFile(cfg, []byte("version: 99\nupdate:\n  proxy: http://proxy.example.com:3128\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := updateProxy(cfg, &errb); err != nil || p != "http://proxy.example.com:3128" {
		t.Fatalf("%q %v", p, err)
	}
}
