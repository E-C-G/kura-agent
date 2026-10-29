// Package docker parle à l'API de Docker Engine par son socket Unix, pour la préparation
// docker_volume : emplacement d'un volume, conteneurs qui le montent, gel et dégel.
//
// Le client ne sait envoyer que ces requêtes (version négociée par GET /version) :
//
//	GET  /volumes/<nom>
//	GET  /containers/json?all=1&filters={"volume":[...]} (ou {"id":[...]})
//	POST /containers/<id>/pause
//	POST /containers/<id>/unpause
//
// Aucune création, suppression ni exécution : le socket donne l'équivalent des droits
// root, l'agent n'en use que pour geler et dégeler.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// minAPI : version de l'API la plus ancienne acceptée (Docker 20.10).
const minAPI = "1.41"

// Client : client de l'API Docker par un socket Unix.
type Client struct {
	socket string
	http   *http.Client
	api    string // version négociée, ex. 1.45
}

// New prépare un client pour le socket ; rien n'est envoyé avant le premier appel.
func New(socket string) *Client {
	return &Client{socket: socket, http: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
			// Le socket est local : ni proxy, ni connexions gardées entre les exécutions.
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		// Jamais de redirection : l'API n'en fait pas.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

var apiPattern = regexp.MustCompile(`^1\.(\d+)$`)

// Negotiate lit la version de l'API et vérifie qu'elle convient.
func (c *Client) Negotiate(ctx context.Context) (engine string, err error) {
	var v struct {
		Version    string `json:"Version"`
		APIVersion string `json:"ApiVersion"`
		Os         string `json:"Os"`
	}
	if err := c.get(ctx, "/version", nil, &v); err != nil {
		return "", err
	}
	m := apiPattern.FindStringSubmatch(v.APIVersion)
	if m == nil || !atLeast(v.APIVersion, minAPI) {
		return v.Version, fmt.Errorf("API Docker %q : %s ou plus récente attendue (Docker 20.10 ou plus)", v.APIVersion, minAPI)
	}
	if v.Os != "" && v.Os != "linux" {
		return v.Version, fmt.Errorf("Docker %s sur %s : seul Docker Engine sous Linux est pris en charge", v.Version, v.Os)
	}
	c.api = v.APIVersion
	return v.Version, nil
}

func atLeast(v, min string) bool {
	var a, b int
	fmt.Sscanf(strings.TrimPrefix(v, "1."), "%d", &a)
	fmt.Sscanf(strings.TrimPrefix(min, "1."), "%d", &b)
	return a >= b
}

// Volume : pilote et emplacement d'un volume.
type Volume struct {
	Name       string `json:"Name"`
	Driver     string `json:"Driver"`
	Mountpoint string `json:"Mountpoint"`
}

// ErrNotFound : volume ou conteneur inconnu de Docker.
var ErrNotFound = errors.New("introuvable")

// Volume lit un volume.
func (c *Client) Volume(ctx context.Context, name string) (Volume, error) {
	var v Volume
	err := c.get(ctx, "/volumes/"+url.PathEscape(name), nil, &v)
	return v, err
}

// Container : un conteneur, tel que la liste le donne.
type Container struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	State string   `json:"State"` // running, paused, exited…
}

// Name : nom lisible (sans la barre initiale), ou identifiant court.
func (k Container) Name() string {
	if len(k.Names) > 0 {
		return strings.TrimPrefix(k.Names[0], "/")
	}
	return k.Short()
}

// Short : identifiant court (12 caractères).
func (k Container) Short() string {
	if len(k.ID) > 12 {
		return k.ID[:12]
	}
	return k.ID
}

// Using : conteneurs (arrêtés compris) qui montent l'un des volumes.
func (c *Client) Using(ctx context.Context, volumes []string) ([]Container, error) {
	return c.list(ctx, map[string][]string{"volume": volumes})
}

// ByID : état actuel des conteneurs d'identifiants ids.
func (c *Client) ByID(ctx context.Context, ids []string) ([]Container, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return c.list(ctx, map[string][]string{"id": ids})
}

func (c *Client) list(ctx context.Context, filters map[string][]string) ([]Container, error) {
	f, err := json.Marshal(filters)
	if err != nil {
		return nil, err
	}
	var out []Container
	err = c.get(ctx, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, &out)
	return out, err
}

// Pause gèle un conteneur.
func (c *Client) Pause(ctx context.Context, id string) error {
	return c.post(ctx, "/containers/"+url.PathEscape(id)+"/pause")
}

// Unpause dégèle un conteneur.
func (c *Client) Unpause(ctx context.Context, id string) error {
	return c.post(ctx, "/containers/"+url.PathEscape(id)+"/unpause")
}

// APIError : réponse d'erreur de l'API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API Docker : %d %s", e.Status, e.Message)
}

// Is : une réponse 404 est ErrNotFound.
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

func (c *Client) url(path string, q url.Values) string {
	p := path
	if c.api != "" && path != "/version" {
		p = "/v" + c.api + path
	}
	u := url.URL{Scheme: "http", Host: "docker", Path: p, RawQuery: q.Encode()}
	return u.String()
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path, q), nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) post(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path, nil), nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("API Docker (%s) injoignable : %w", c.socket, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &m)
		if m.Message == "" {
			m.Message = strings.TrimSpace(string(body))
		}
		return &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("réponse illisible de l'API Docker : %w", err)
	}
	return nil
}
