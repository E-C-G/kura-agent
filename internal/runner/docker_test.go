//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"

	"github.com/E-C-G/kura-agent/internal/docker"
	"github.com/E-C-G/kura-agent/internal/prepare"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// fakeEngine : API Docker minimale, un conteneur et ses changements d'état.
type fakeEngine struct {
	mu    sync.Mutex
	root  string
	state map[string]string
	calls []string
}

func (f *fakeEngine) Negotiate(context.Context) (string, error) { return "27.3.1", nil }
func (f *fakeEngine) Volume(_ context.Context, n string) (docker.Volume, error) {
	return docker.Volume{Name: n, Driver: "local", Mountpoint: filepath.Join(f.root, "volumes", n, "_data")}, nil
}
func (f *fakeEngine) list(ids []string) []docker.Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.Container
	for id, st := range f.state {
		if ids == nil || slices.Contains(ids, id) {
			out = append(out, docker.Container{ID: id, Names: []string{"/" + id}, State: st})
		}
	}
	return out
}
func (f *fakeEngine) Using(context.Context, []string) ([]docker.Container, error) {
	return f.list(nil), nil
}
func (f *fakeEngine) ByID(_ context.Context, ids []string) ([]docker.Container, error) {
	return f.list(ids), nil
}
func (f *fakeEngine) do(op, id, st string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op+" "+id)
	f.state[id] = st
	return nil
}
func (f *fakeEngine) Pause(_ context.Context, id string) error { return f.do("pause", id, "paused") }
func (f *fakeEngine) Unpause(_ context.Context, id string) error {
	return f.do("unpause", id, "running")
}

// withDocker : volume appli_db archivé, chiffré par age au fil de l'eau, envoyé par le job.
func withDocker(b *bench) *fakeEngine {
	f := &fakeEngine{root: filepath.Join(b.dir, "docker"), state: map[string]string{"appli-db": "running"}}
	mk(b.t, filepath.Join(f.root, "volumes", "appli_db", "_data", "base", "PG_VERSION"))
	b.docker = func(string) prepare.DockerAPI { return f }
	b.paused = filepath.Join(b.dir, "docker-paused.json")
	id, _ := age.GenerateX25519Identity()
	b.cfg.Prepare = []config.Prepare{{ID: "appli", Type: config.PrepareDocker, Volumes: []string{"appli_db"}},
		{ID: "appli-chiffre", Type: config.PrepareAge, Input: "appli", Recipients: []config.Recipient{{Age: id.Recipient().String()}}}}
	b.cfg.Jobs = append(b.cfg.Jobs, config.Job{ID: "appli", Source: "${STAGING:appli-chiffre}", Destination: "DOCKER/APPLI", Delete: true, Prepare: "appli-chiffre"})
	b.cfg.ApplyDefaults()
	b.cfg.ResolveStaging(filepath.Join(b.dir, "staging"))
	if err := b.cfg.Validate(config.ValidateOptions{}); err != nil {
		b.t.Fatal(err)
	}
	return f
}

// Volume gelé, lu, chiffré au fil de l'eau : seul le chiffré est dans la zone à l'envoi.
func TestDockerPrepare(t *testing.T) {
	b := newBench(t)
	f := withDocker(b)
	staging := filepath.Join(b.dir, "staging")
	var sent []string
	b.aws.behave["DOCKER/APPLI"] = func(dry bool, n int) (string, int) {
		if !dry {
			sent = listDir(staging)
		}
		return "", 0
	}
	st := b.run(context.Background(), Options{JobIDs: []string{"appli"}})
	if st.ExitCode != ExitOK || len(st.Prepares) != 1 || st.Prepares[0].ID != "appli-chiffre" {
		t.Fatalf("code %d, préparations %+v", st.ExitCode, st.Prepares)
	}
	if p := st.Prepares[0]; !slices.Equal(p.Paused, []string{"appli-db"}) || len(p.Volumes) != 1 {
		t.Fatalf("bilan : %+v", p)
	}
	if !slices.Equal(sent, []string{"appli-chiffre/appli_db.tar.gz.age"}) {
		t.Fatalf("zone pendant l'envoi : %v", sent)
	}
	if !slices.Equal(f.calls, []string{"pause appli-db", "unpause appli-db"}) {
		t.Fatalf("appels : %v", f.calls)
	}
}

// Agent tué pendant un gel : l'exécution suivante dégèle d'abord, et le signale.
func TestDockerRecoveryAtStart(t *testing.T) {
	b := newBench(t)
	f := withDocker(b)
	f.state["appli-db"] = "paused"
	data, _ := json.Marshal([]prepare.PausedEntry{{ID: "appli-db", Name: "appli-db", Prepare: "appli", Socket: config.DockerSocket}})
	os.WriteFile(b.paused, data, 0o600)
	st := b.run(context.Background(), Options{JobIDs: []string{"appli"}})
	if st.ExitCode != ExitWarning || !strings.Contains(st.Message, "dégelés au démarrage : appli-db") {
		t.Fatalf("code %d, message %q", st.ExitCode, st.Message)
	}
	if !slices.Equal(f.calls, []string{"unpause appli-db", "pause appli-db", "unpause appli-db"}) {
		t.Fatalf("appels : %v", f.calls)
	}
	if _, err := os.Stat(b.paused); !os.IsNotExist(err) {
		t.Fatal("fichier de reprise resté")
	}
}
