//go:build !windows

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeEngine : faux Docker Engine sur un socket Unix ; note chaque requête.
type fakeEngine struct {
	mu       sync.Mutex
	requests []string
	api      string
	states   map[string]string
}

func (f *fakeEngine) serve(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("socket Unix indisponible : %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		path := r.URL.Path
		if i := strings.Index(path[1:], "/"); strings.HasPrefix(path, "/v1.") && i > 0 {
			path = path[i+1:]
		}
		switch {
		case path == "/version":
			json.NewEncoder(w).Encode(map[string]string{"Version": "27.3.1", "ApiVersion": f.api, "Os": "linux"})
		case path == "/volumes/appli_db":
			json.NewEncoder(w).Encode(Volume{Name: "appli_db", Driver: "local", Mountpoint: "/var/lib/docker/volumes/appli_db/_data"})
		case strings.HasPrefix(path, "/volumes/"):
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "no such volume"})
		case path == "/containers/json":
			var filt map[string][]string
			json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filt)
			var out []Container
			for id, st := range f.states {
				if ids, ok := filt["id"]; ok && !slices.Contains(ids, id) {
					continue
				}
				out = append(out, Container{ID: id, Names: []string{"/" + id + "-nom"}, State: st})
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(path, "/pause") && r.Method == http.MethodPost:
			f.states[strings.Split(path, "/")[2]] = "paused"
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/unpause") && r.Method == http.MethodPost:
			f.states[strings.Split(path, "/")[2]] = "running"
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestClient(t *testing.T) {
	f := &fakeEngine{api: "1.47", states: map[string]string{"abc123def456789": "running"}}
	c := New(f.serve(t))
	ctx := context.Background()
	if v, err := c.Negotiate(ctx); err != nil || v != "27.3.1" {
		t.Fatalf("%q %v", v, err)
	}
	v, err := c.Volume(ctx, "appli_db")
	if err != nil || v.Driver != "local" || v.Mountpoint != "/var/lib/docker/volumes/appli_db/_data" {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := c.Volume(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("volume absent : %v", err)
	}
	ks, err := c.Using(ctx, []string{"appli_db"})
	if err != nil || len(ks) != 1 || ks[0].Name() != "abc123def456789-nom" || ks[0].Short() != "abc123def456" {
		t.Fatalf("%+v %v", ks, err)
	}
	if err := c.Pause(ctx, ks[0].ID); err != nil {
		t.Fatal(err)
	}
	if now, _ := c.ByID(ctx, []string{ks[0].ID}); len(now) != 1 || now[0].State != "paused" {
		t.Fatalf("%+v", now)
	}
	if err := c.Unpause(ctx, ks[0].ID); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /version", "GET /v1.47/volumes/appli_db", "GET /v1.47/volumes/absent", "GET /v1.47/containers/json",
		"POST /v1.47/containers/abc123def456789/pause", "GET /v1.47/containers/json", "POST /v1.47/containers/abc123def456789/unpause"}
	if !slices.Equal(f.requests, want) {
		t.Fatalf("requêtes :\n%v\nattendu :\n%v", f.requests, want)
	}
}

func TestClientRefusesOldAPI(t *testing.T) {
	f := &fakeEngine{api: "1.40"}
	if _, err := New(f.serve(t)).Negotiate(context.Background()); err == nil || !strings.Contains(err.Error(), "1.41") {
		t.Fatalf("%v", err)
	}
}

func TestCheckSocket(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "fichier")
	os.WriteFile(plain, nil, 0o600)
	if err := CheckSocket(plain); err == nil || !strings.Contains(err.Error(), "pas un socket") {
		t.Fatalf("fichier ordinaire accepté : %v", err)
	}
	link := filepath.Join(dir, "lien")
	os.Symlink(plain, link)
	if err := CheckSocket(link); err == nil {
		t.Fatal("lien accepté")
	}
	if err := CheckSocket(filepath.Join(dir, "absent")); err == nil || !strings.Contains(err.Error(), "Docker Engine installé") {
		t.Fatalf("%v", err)
	}
	f := &fakeEngine{api: "1.47"}
	sock := f.serve(t)
	err := CheckSocket(sock)
	if os.Geteuid() == 0 {
		os.Chmod(sock, 0o666)
		if err := CheckSocket(sock); err == nil || !strings.Contains(err.Error(), "modifiable par tous") {
			t.Fatalf("socket ouvert à tous accepté : %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "pas à root") {
		t.Fatalf("socket d'un utilisateur accepté : %v", err)
	}
}
