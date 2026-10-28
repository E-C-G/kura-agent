// Package selfupdate cherche une version plus récente de kura-agent dans les releases
// GitHub du dépôt et vérifie l'attestation de release que GitHub signe pour chaque
// release immuable : tag, commit et empreinte SHA-256 de chaque fichier publié.
//
// Rien n'est installé ici (lot 2) : Check dit si une version plus récente existe et
// si l'archive de ce système est bien attestée par GitHub.
//
// Contrôles, dans l'ordre :
//
//	release      ni brouillon ni préversion, immuable (champ immutable), tag vX.Y.Z
//	archive      celle du système (Windows : zip, Ubuntu : deb, macOS : tar.gz),
//	             empreinte SHA-256 annoncée par l'API
//	attestation  récupérée par cette empreinte (prédicat release), vérifiée par
//	             sigstore-go contre la racine de confiance de GitHub embarquée :
//	             chaîne du certificat, horodatage signé, identité
//	             https://dotcom.releases.github.com ; puis dépôt, tag et sujet
//	             (nom et empreinte de l'archive) lus dans l'attestation vérifiée
package selfupdate

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Repo : dépôt des releases de l'agent.
const Repo = "E-C-G/kura-agent"

// DefaultAPI : API REST de GitHub.
const DefaultAPI = "https://api.github.com"

// Limites de taille des réponses de l'API : rien de légitime n'en approche.
const (
	maxReleaseJSON     = 2 << 20
	maxAttestationJSON = 8 << 20
)

// Asset est un fichier d'une release.
type Asset struct {
	Name   string
	Size   int64
	SHA256 string // hexadécimal, minuscules
	URL    string // browser_download_url
}

// Release est une release publiée.
type Release struct {
	Tag       string
	Version   string // tag sans le v
	Published time.Time
	NotesURL  string
	Immutable bool
	Assets    []Asset
}

// Result est la réponse de Check.
type Result struct {
	Current  string  // version en place
	Latest   Release // dernière release (ou celle demandée)
	Newer    bool    // Latest est plus récente que Current
	Asset    Asset   // archive de ce système, attestation vérifiée
	Verified bool    // attestation de Asset vérifiée
}

// Client interroge GitHub. Zéro : API publique, client HTTP par défaut de NewHTTPClient,
// racine de confiance embarquée.
type Client struct {
	HTTP        *http.Client
	API         string // DefaultAPI si vide (tests : serveur local)
	TrustedRoot []byte // racine de confiance ; vide : celle de GitHub, embarquée
	UserAgent   string
}

var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ParseVersion lit « 1.2.3 » ou « v1.2.3 ».
func ParseVersion(v string) ([3]int, error) {
	var out [3]int
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	m := tagPattern.FindStringSubmatch(v)
	if m == nil {
		return out, fmt.Errorf("version %q : vX.Y.Z attendu", strings.TrimPrefix(v, "v"))
	}
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return out, fmt.Errorf("version %q : %w", v, err)
		}
		out[i] = n
	}
	return out, nil
}

// Compare renvoie -1, 0 ou 1 selon que a est plus ancienne, égale ou plus récente que b.
func Compare(a, b string) (int, error) {
	va, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range va {
		switch {
		case va[i] < vb[i]:
			return -1, nil
		case va[i] > vb[i]:
			return 1, nil
		}
	}
	return 0, nil
}

// AssetName : archive de la release pour ce système (nom exact publié par release.yml).
func AssetName(version, goos, goarch string) (string, error) {
	switch {
	case goos == "windows" && goarch == "amd64":
		return "kura-agent_" + version + "_windows_amd64.zip", nil
	case goos == "linux" && (goarch == "amd64" || goarch == "arm64"):
		return "kura-agent_" + version + "_" + goarch + ".deb", nil
	case goos == "darwin" && (goarch == "amd64" || goarch == "arm64"):
		return "kura-agent_" + version + "_darwin_" + goarch + ".tar.gz", nil
	}
	return "", fmt.Errorf("aucune archive publiée pour %s/%s", goos, goarch)
}

// Options de Check.
type Options struct {
	Current string // version en place (main.version)
	Tag     string // vX.Y.Z visé ; vide : dernière release
	GOOS    string
	GOARCH  string
}

// ErrDevVersion : exécutable de développement, sans version à comparer.
var ErrDevVersion = errors.New("version de développement : comparaison impossible")

// Check cherche la release, choisit l'archive du système et vérifie son attestation.
// Une release non attestée ou invalide est une erreur, même si elle n'est pas plus
// récente : la vérification ne dépend pas de la comparaison.
func (c *Client) Check(ctx context.Context, o Options) (Result, error) {
	res := Result{Current: o.Current}
	rel, err := c.release(ctx, o.Tag)
	if err != nil {
		return res, err
	}
	res.Latest = rel
	name, err := AssetName(rel.Version, o.GOOS, o.GOARCH)
	if err != nil {
		return res, err
	}
	var asset *Asset
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			asset = &rel.Assets[i]
		}
	}
	if asset == nil {
		return res, fmt.Errorf("release %s : archive %s absente", rel.Tag, name)
	}
	if asset.SHA256 == "" {
		return res, fmt.Errorf("release %s : empreinte de %s non fournie par GitHub", rel.Tag, name)
	}
	if err := c.verifyAsset(ctx, rel, *asset); err != nil {
		return res, err
	}
	res.Asset, res.Verified = *asset, true
	if o.Current == "" || o.Current == "dev" {
		return res, ErrDevVersion
	}
	cmp, err := Compare(rel.Version, o.Current)
	if err != nil {
		return res, fmt.Errorf("version en place : %w", err)
	}
	res.Newer = cmp > 0
	return res, nil
}

type apiRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Immutable   bool      `json:"immutable"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
		URL    string `json:"browser_download_url"`
	} `json:"assets"`
}

func (c *Client) release(ctx context.Context, tag string) (Release, error) {
	path := "/repos/" + Repo + "/releases/latest"
	if tag != "" {
		if !tagPattern.MatchString(tag) {
			return Release{}, fmt.Errorf("version %q : vX.Y.Z attendu", tag)
		}
		path = "/repos/" + Repo + "/releases/tags/" + tag
	}
	var r apiRelease
	if err := c.getJSON(ctx, path, maxReleaseJSON, &r); err != nil {
		return Release{}, fmt.Errorf("release : %w", err)
	}
	switch {
	case !tagPattern.MatchString(r.TagName):
		return Release{}, fmt.Errorf("release %q : tag vX.Y.Z attendu", r.TagName)
	case tag != "" && r.TagName != tag:
		return Release{}, fmt.Errorf("release %s demandée, %s reçue", tag, r.TagName)
	case r.Draft:
		return Release{}, fmt.Errorf("release %s : brouillon, refusée", r.TagName)
	case r.Prerelease:
		return Release{}, fmt.Errorf("release %s : préversion, refusée", r.TagName)
	case !r.Immutable:
		return Release{}, fmt.Errorf("release %s : non immuable, refusée (réglage « releases immuables » du dépôt)", r.TagName)
	}
	rel := Release{Tag: r.TagName, Version: strings.TrimPrefix(r.TagName, "v"), Published: r.PublishedAt,
		NotesURL: r.HTMLURL, Immutable: r.Immutable}
	for _, a := range r.Assets {
		sum := ""
		if hexSum, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && isSHA256(hexSum) {
			sum = strings.ToLower(hexSum)
		}
		rel.Assets = append(rel.Assets, Asset{Name: a.Name, Size: a.Size, SHA256: sum, URL: a.URL})
	}
	return rel, nil
}

func isSHA256(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32
}

func (c *Client) api() string {
	if c.API != "" {
		return strings.TrimRight(c.API, "/")
	}
	return DefaultAPI
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	hc, _ := NewHTTPClient("")
	return hc
}

// getJSON lit une ressource de l'API GitHub (sans jeton : dépôt public).
func (c *Client) getJSON(ctx context.Context, path string, limit int64, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	ua := c.UserAgent
	if ua == "" {
		ua = "kura-agent"
	}
	req.Header.Set("User-Agent", ua)
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return fmt.Errorf("%s : réponse trop volumineuse", path)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s : introuvable (404)", path)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%s : refusé par GitHub (%d), limite de requêtes atteinte ? Réessayer plus tard", path, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s : réponse %d de GitHub", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s : réponse illisible : %w", path, err)
	}
	return nil
}

// allowedHosts : hôtes que le client suit (API, puis téléchargements des releases).
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

// NewHTTPClient : HTTPS seulement, redirections limitées aux hôtes de GitHub, délai de
// 30 s. proxy : URL du proxy (update.proxy) ; vide : variables d'environnement.
func NewHTTPClient(proxy string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return nil, fmt.Errorf("update.proxy : URL de proxy invalide %q", proxy)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("trop de redirections")
			}
			if req.URL.Scheme != "https" || !allowedHosts[req.URL.Hostname()] {
				return fmt.Errorf("redirection refusée vers %s://%s", req.URL.Scheme, req.URL.Host)
			}
			return nil
		},
	}, nil
}
