package selfupdate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// Données réelles de la release v0.8.2 (immuable) : réponse de l'API des releases
// (réduite aux champs lus) et attestation de release signée par GitHub.
const (
	zipName = "kura-agent_0.8.2_windows_amd64.zip"
	zipSum  = "bc1d1e62dbb67f56fa58a8fd174e4e0c51ab15feea53af2bddfb5a5b0346c1fb"
	debSum  = "8f29f5ba2e24f465aa74f2eb1141df5e36e1d0cf9ab73fd30a6ff9c4d730f6b9"
)

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bundleJSON(t *testing.T) []byte {
	t.Helper()
	var list apiAttestations
	if err := json.Unmarshal(readFile(t, "attestations_v0.8.2.json"), &list); err != nil || len(list.Attestations) != 1 {
		t.Fatalf("données de test : %v", err)
	}
	return list.Attestations[0].Bundle
}

func githubRoot(t *testing.T) *root.TrustedRoot {
	t.Helper()
	tr, err := root.NewTrustedRootFromJSON(GitHubTrustedRoot())
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestVerifyReleaseBundle(t *testing.T) {
	tr, b := githubRoot(t), bundleJSON(t)
	if err := VerifyReleaseBundle(tr, b, "v0.8.2", zipName, zipSum); err != nil {
		t.Fatalf("attestation réelle de la v0.8.2 refusée : %v", err)
	}
	if err := VerifyReleaseBundle(tr, b, "v0.8.2", "kura-agent_0.8.2_amd64.deb", debSum); err != nil {
		t.Fatalf("paquet deb : %v", err)
	}
	for _, c := range []struct{ tag, name, sum, want string }{
		{"v0.8.3", zipName, zipSum, "tag"},
		{"v0.8.2", "kura-agent_0.8.2_windows_amd64.exe", zipSum, "absent"},
		{"v0.8.2", zipName, debSum, "absent"},
		{"v0.8.2", zipName, strings.Repeat("0", 64), "signature"},
		{"v0.8.2", zipName, "pas-hexa", "invalide"},
	} {
		if err := VerifyReleaseBundle(tr, b, c.tag, c.name, c.sum); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v : %v", c, err)
		}
	}
}

// Une seule empreinte changée dans la déclaration signée : la signature ne tient plus.
func TestTamperedAttestation(t *testing.T) {
	tr, b := githubRoot(t), bundleJSON(t)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	env := raw["dsseEnvelope"].(map[string]any)
	payload, err := base64.StdEncoding.DecodeString(env["payload"].(string))
	if err != nil {
		t.Fatal(err)
	}
	evil := strings.Repeat("e", 64)
	forged := bytes.Replace(payload, []byte(zipSum), []byte(evil), 1)
	if bytes.Equal(forged, payload) {
		t.Fatal("empreinte absente de la déclaration")
	}
	env["payload"] = base64.StdEncoding.EncodeToString(forged)
	b2, _ := json.Marshal(raw)
	if err := VerifyReleaseBundle(tr, b2, "v0.8.2", zipName, evil); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("attestation falsifiée acceptée : %v", err)
	}
}

// Une autre racine de confiance (Sigstore public, par exemple) ne valide pas une
// attestation de GitHub.
func TestOtherTrustRootRefused(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal(GitHubTrustedRoot(), &raw); err != nil {
		t.Fatal(err)
	}
	cas := raw["certificateAuthorities"].([]any)
	raw["certificateAuthorities"] = cas[:1] // autorité de 2023-2024 seulement
	b, _ := json.Marshal(raw)
	tr, err := root.NewTrustedRootFromJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyReleaseBundle(tr, bundleJSON(t), "v0.8.2", zipName, zipSum); err == nil {
		t.Fatal("attestation acceptée sans l'autorité qui l'a signée")
	}
}

// fakeGitHub sert la release v0.8.2 et son attestation ; mutate modifie la release.
func fakeGitHub(t *testing.T, mutate func(map[string]any)) *httptest.Server {
	t.Helper()
	rel := readFile(t, "release_v0.8.2.json")
	att := readFile(t, "attestations_v0.8.2.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Accept") != "application/vnd.github+json" {
			http.Error(w, "en-têtes", http.StatusBadRequest)
			return
		}
		switch {
		case r.URL.Path == "/repos/E-C-G/kura-agent/releases/latest", r.URL.Path == "/repos/E-C-G/kura-agent/releases/tags/v0.8.2":
			var m map[string]any
			_ = json.Unmarshal(rel, &m)
			if mutate != nil {
				mutate(m)
			}
			_ = json.NewEncoder(w).Encode(m)
		case strings.HasPrefix(r.URL.Path, "/repos/E-C-G/kura-agent/attestations/sha256:"):
			if r.URL.Query().Get("predicate_type") != "release" {
				http.Error(w, "prédicat", http.StatusBadRequest)
				return
			}
			// Comme l'API : l'attestation de la release répond pour chacun de ses sujets.
			_, _ = w.Write(att)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheck(t *testing.T) {
	srv := fakeGitHub(t, nil)
	c := &Client{API: srv.URL, HTTP: srv.Client()}
	ctx := context.Background()
	win := Options{Current: "0.8.1", GOOS: "windows", GOARCH: "amd64"}
	res, err := c.Check(ctx, win)
	if err != nil || !res.Newer || !res.Verified || res.Asset.Name != zipName || res.Latest.Tag != "v0.8.2" {
		t.Fatalf("0.8.1 → 0.8.2 : %+v %v", res, err)
	}
	for _, cur := range []string{"0.8.2", "0.9.0"} {
		o := win
		o.Current = cur
		if res, err := c.Check(ctx, o); err != nil || res.Newer || !res.Verified {
			t.Errorf("en place %s : %+v %v", cur, res, err)
		}
	}
	o := win
	o.Current = "dev"
	if res, err := c.Check(ctx, o); !errors.Is(err, ErrDevVersion) || !res.Verified {
		t.Errorf("version de développement : %+v %v", res, err)
	}
	// Ubuntu arm64 : paquet deb ; macOS : tar.gz.
	for _, o := range []Options{{Current: "0.8.1", GOOS: "linux", GOARCH: "arm64"}, {Current: "0.8.1", GOOS: "darwin", GOARCH: "arm64"}} {
		if res, err := c.Check(ctx, o); err != nil || !res.Verified {
			t.Errorf("%s/%s : %+v %v", o.GOOS, o.GOARCH, res, err)
		}
	}
	o = win
	o.Tag = "v0.8.2"
	if res, err := c.Check(ctx, o); err != nil || res.Latest.Tag != "v0.8.2" {
		t.Errorf("version visée : %+v %v", res, err)
	}
	o.Tag = "0.8.2"
	if _, err := c.Check(ctx, o); err == nil {
		t.Error("tag sans v accepté")
	}
}

func TestCheckRefusals(t *testing.T) {
	win := Options{Current: "0.8.1", GOOS: "windows", GOARCH: "amd64"}
	for _, c := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"non immuable", func(m map[string]any) { m["immutable"] = false }, "non immuable"},
		{"préversion", func(m map[string]any) { m["prerelease"] = true }, "préversion"},
		{"brouillon", func(m map[string]any) { m["draft"] = true }, "brouillon"},
		{"tag", func(m map[string]any) { m["tag_name"] = "v0.8.2-rc1" }, "vX.Y.Z"},
		{"sans empreinte", func(m map[string]any) {
			for _, a := range m["assets"].([]any) {
				delete(a.(map[string]any), "digest")
			}
		}, "empreinte"},
		{"archive absente", func(m map[string]any) { m["assets"] = []any{} }, "absente"},
		// L'API annonce une autre empreinte que celle attestée.
		{"empreinte non attestée", func(m map[string]any) {
			for _, a := range m["assets"].([]any) {
				if a.(map[string]any)["name"] == zipName {
					a.(map[string]any)["digest"] = "sha256:" + debSum
				}
			}
		}, "absent de l'attestation"},
	} {
		srv := fakeGitHub(t, c.mutate)
		cl := &Client{API: srv.URL, HTTP: srv.Client()}
		if _, err := cl.Check(context.Background(), win); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s : %v", c.name, err)
		}
	}
	// Aucune attestation (release publiée avant les releases immuables).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/attestations/") {
			_, _ = w.Write([]byte(`{"attestations":[]}`))
			return
		}
		_, _ = w.Write(readFile(t, "release_v0.8.2.json"))
	}))
	defer srv.Close()
	cl := &Client{API: srv.URL, HTTP: srv.Client()}
	if _, err := cl.Check(context.Background(), win); err == nil || !strings.Contains(err.Error(), "aucune attestation") {
		t.Errorf("sans attestation : %v", err)
	}
	// Limite de requêtes de GitHub.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv2.Close()
	cl = &Client{API: srv2.URL, HTTP: srv2.Client()}
	if _, err := cl.Check(context.Background(), win); err == nil || !strings.Contains(err.Error(), "limite") {
		t.Errorf("403 : %v", err)
	}
}

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.8.2", "0.8.1", 1}, {"v0.8.2", "0.8.2", 0}, {"0.9.0", "0.10.0", -1}, {"1.0.0", "0.99.99", 1},
	} {
		if got, err := Compare(c.a, c.b); err != nil || got != c.want {
			t.Errorf("%s ? %s : %d %v", c.a, c.b, got, err)
		}
	}
	for _, bad := range []string{"", "dev", "0.8", "0.8.2-rc1", "01.2.3", "v0.8.2 "} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q accepté", bad)
		}
	}
	for _, c := range []struct{ goos, goarch, want string }{
		{"windows", "amd64", "kura-agent_1.2.3_windows_amd64.zip"},
		{"linux", "amd64", "kura-agent_1.2.3_amd64.deb"},
		{"darwin", "amd64", "kura-agent_1.2.3_darwin_amd64.tar.gz"},
	} {
		if got, err := AssetName("1.2.3", c.goos, c.goarch); err != nil || got != c.want {
			t.Errorf("%s/%s : %s %v", c.goos, c.goarch, got, err)
		}
	}
	if _, err := AssetName("1.2.3", "windows", "arm64"); err == nil {
		t.Error("windows/arm64 : aucune archive publiée")
	}
}

func TestHTTPClient(t *testing.T) {
	if _, err := NewHTTPClient("socks5://proxy:1080"); err == nil {
		t.Error("proxy socks5 accepté")
	}
	hc, err := NewHTTPClient("http://proxy.example.com:3128")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/", nil)
	if u, err := hc.Transport.(*http.Transport).Proxy(req); err != nil || u.Host != "proxy.example.com:3128" {
		t.Errorf("proxy : %v %v", u, err)
	}
	// Redirections : HTTPS vers les hôtes de GitHub seulement.
	for _, c := range []struct {
		to string
		ok bool
	}{
		{"https://objects.githubusercontent.com/x", true},
		{"https://release-assets.githubusercontent.com/x", true},
		{"http://objects.githubusercontent.com/x", false},
		{"https://example.com/x", false},
	} {
		req, _ := http.NewRequest(http.MethodGet, c.to, nil)
		if err := hc.CheckRedirect(req, []*http.Request{{}}); (err == nil) != c.ok {
			t.Errorf("redirection vers %s : %v", c.to, err)
		}
	}
}
