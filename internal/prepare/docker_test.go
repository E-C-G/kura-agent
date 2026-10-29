//go:build !windows

package prepare

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/docker"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// fakeDocker joue l'API Docker : volumes sous root, conteneurs et leurs états.
type fakeDocker struct {
	mu         sync.Mutex
	root       string
	containers []docker.Container
	calls      []string
	noFreeze   bool   // le gel ne prend pas
	onUnpause  func() // appelé avant chaque dégel
}

func (f *fakeDocker) log(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeDocker) Negotiate(context.Context) (string, error) { return "27.3.1", nil }
func (f *fakeDocker) Volume(_ context.Context, name string) (docker.Volume, error) {
	dir := filepath.Join(f.root, "volumes", name, "_data")
	if _, err := os.Stat(dir); err != nil {
		return docker.Volume{}, &docker.APIError{Status: 404, Message: "no such volume"}
	}
	return docker.Volume{Name: name, Driver: "local", Mountpoint: dir}, nil
}
func (f *fakeDocker) Using(context.Context, []string) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.containers), nil
}
func (f *fakeDocker) ByID(_ context.Context, ids []string) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.Container
	for _, k := range f.containers {
		if slices.Contains(ids, k.ID) {
			out = append(out, k)
		}
	}
	return out, nil
}
func (f *fakeDocker) set(id, st string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.containers {
		if f.containers[i].ID == id {
			f.containers[i].State = st
		}
	}
}
func (f *fakeDocker) Pause(_ context.Context, id string) error {
	f.log("pause " + id)
	if !f.noFreeze {
		f.set(id, "paused")
	}
	return nil
}
func (f *fakeDocker) Unpause(_ context.Context, id string) error {
	f.log("unpause " + id)
	if f.onUnpause != nil {
		f.onUnpause()
	}
	f.set(id, "running")
	return nil
}
func (f *fakeDocker) state(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.containers {
		if k.ID == id {
			return k.State
		}
	}
	return ""
}

type dockerFixture struct {
	cfg     *config.Config
	staging string
	fake    *fakeDocker
	vol     string // dossier du volume appli_db
	paused  string // fichier de reprise
}

func newDockerFixture(t *testing.T) dockerFixture {
	t.Helper()
	dir := t.TempDir()
	f := dockerFixture{staging: filepath.Join(dir, "staging"), paused: filepath.Join(dir, "docker-paused.json")}
	os.MkdirAll(f.staging, 0o700)
	root := filepath.Join(dir, "docker")
	f.vol = filepath.Join(root, "volumes", "appli_db", "_data")
	for name, data := range map[string]string{"base/PG_VERSION": "16", "base/1/table": strings.Repeat("x", 3000), "cache/tmp": "c", "été.txt": "é"} {
		p := filepath.Join(f.vol, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o750)
		os.WriteFile(p, []byte(data), 0o640)
	}
	os.Chmod(filepath.Join(f.vol, "base"), 0o700)
	os.Symlink("/etc/passwd", filepath.Join(f.vol, "dehors"))
	os.Symlink("base/PG_VERSION", filepath.Join(f.vol, "dedans"))
	os.Link(filepath.Join(f.vol, "été.txt"), filepath.Join(f.vol, "dur.txt"))
	syscall.Mkfifo(filepath.Join(f.vol, "tube"), 0o600)
	f.fake = &fakeDocker{root: root, containers: []docker.Container{
		{ID: "aaa", Names: []string{"/appli-db-1"}, State: "running"},
		{ID: "bbb", Names: []string{"/appli-sauvegarde"}, State: "paused"},
		{ID: "ccc", Names: []string{"/appli-ancien"}, State: "exited"},
	}}
	f.cfg = &config.Config{Prepare: []config.Prepare{{ID: "appli", Type: config.PrepareDocker, Volumes: []string{"appli_db"},
		Exclude: []string{"cache/*"}}}}
	f.cfg.ApplyDefaults()
	return f
}

func (f dockerFixture) deps() Deps {
	return Deps{Exec: &fakeDB{}, StagingDir: f.staging, PausedFile: f.paused,
		CheckSocket: func(string) error { return nil },
		Docker:      func(string) DockerAPI { return f.fake }}
}

// tarEntries : entrées de l'archive (nom → en-tête) et contenu des fichiers.
func tarEntries(t *testing.T, path string) (map[string]*tar.Header, map[string]string) {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	hdrs, data := map[string]*tar.Header{}, map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		hdrs[h.Name], data[h.Name] = h, string(b)
	}
	return hdrs, data
}

func TestDockerVolumeArchive(t *testing.T) {
	f := newDockerFixture(t)
	// Le gel tient pendant toute la lecture : au dégel, l'archive est déjà complète.
	readBeforeThaw := -1
	f.fake.onUnpause = func() {
		readBeforeThaw, _ = verifyTarGz(filepath.Join(f.staging, "appli.tmp", "appli_db.tar.gz"))
	}
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], f.deps())
	if ps.Result != state.ResultWarning || !strings.Contains(ps.Message, "tube(s)") {
		t.Fatalf("%+v", ps)
	}
	if !slices.Equal(ps.Paused, []string{"appli-db-1"}) || len(ps.Volumes) != 1 || ps.Volumes[0].Name != "appli_db" {
		t.Fatalf("état : %+v", ps)
	}
	if readBeforeThaw != ps.Volumes[0].Entries {
		t.Fatalf("archive au dégel : %d entrées sur %d", readBeforeThaw, ps.Volumes[0].Entries)
	}
	if got := strings.Join(f.fake.calls, " "); got != "pause aaa unpause aaa" {
		t.Fatalf("appels : %s (le conteneur déjà en pause et l'arrêté ne sont pas touchés)", got)
	}
	if f.fake.state("bbb") != "paused" {
		t.Fatal("conteneur déjà en pause dégelé")
	}
	if _, err := os.Stat(f.paused); !os.IsNotExist(err) {
		t.Fatal("fichier de reprise resté après le dégel")
	}
	hdrs, data := tarEntries(t, filepath.Join(f.staging, "appli", "appli_db.tar.gz"))
	var names []string
	for n := range hdrs {
		names = append(names, n)
	}
	slices.Sort(names)
	want := []string{"./", "base/", "base/1/", "base/1/table", "base/PG_VERSION", "dedans", "dehors", "dur.txt", "été.txt"}
	if !slices.Equal(names, want) {
		t.Fatalf("entrées %v, attendu %v (cache exclu, tube laissé)", names, want)
	}
	if h := hdrs["dehors"]; h.Typeflag != tar.TypeSymlink || h.Linkname != "/etc/passwd" {
		t.Fatalf("lien vers l'extérieur suivi : %+v", h)
	}
	if h := hdrs["dedans"]; h.Typeflag != tar.TypeSymlink || h.Linkname != "base/PG_VERSION" {
		t.Fatalf("lien interne : %+v", h)
	}
	a, b := hdrs["dur.txt"], hdrs["été.txt"]
	if !(a.Typeflag == tar.TypeLink && a.Linkname == "été.txt" || b.Typeflag == tar.TypeLink && b.Linkname == "dur.txt") {
		t.Fatalf("lien physique : %+v %+v", a, b)
	}
	if h := hdrs["base/"]; h.Mode != 0o700 || h.Uid != os.Getuid() {
		t.Fatalf("droits du dossier : mode %o uid %d", h.Mode, h.Uid)
	}
	if h := hdrs["base/1/table"]; h.Mode != 0o640 || data["base/1/table"] != strings.Repeat("x", 3000) {
		t.Fatalf("fichier : mode %o, %d octets", h.Mode, len(data["base/1/table"]))
	}
}

// Gel qui ne prend pas : rien n'est lu, les conteneurs gelés sont dégelés.
func TestDockerFreezeFails(t *testing.T) {
	f := newDockerFixture(t)
	f.fake.noFreeze = true
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], f.deps())
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "n'est pas gelé") {
		t.Fatalf("%+v", ps)
	}
	if got := strings.Join(f.fake.calls, " "); got != "pause aaa unpause aaa" {
		t.Fatalf("appels : %s", got)
	}
	if left, _ := os.ReadDir(f.staging); len(left) != 0 {
		t.Fatalf("zone non vide : %v", left)
	}
}

// max_pause dépassé : lecture arrêtée, dégel, échec avec la cause.
func TestDockerMaxPause(t *testing.T) {
	f := newDockerFixture(t)
	f.cfg.Prepare[0].MaxPause.Duration = time.Nanosecond
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], f.deps())
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "max_pause") {
		t.Fatalf("%+v", ps)
	}
	if f.fake.state("aaa") != "running" {
		t.Fatal("conteneur resté gelé")
	}
}

// Volume inconnu, pilote refusé : échec sans rien geler.
func TestDockerVolumeRefusals(t *testing.T) {
	f := newDockerFixture(t)
	f.cfg.Prepare[0].Volumes = []string{"appli_db", "absent"}
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], f.deps())
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "absent") || len(f.fake.calls) != 0 {
		t.Fatalf("%+v %v", ps, f.fake.calls)
	}
	if !mountpointOK("/var/lib/docker/volumes/v/_data", "v") || mountpointOK("/var/lib/docker/volumes/v/_data/../../x", "v") ||
		mountpointOK("/home/alice/v/_data", "v") || mountpointOK("/volumes/v/_data", "v") {
		t.Fatal("contrôle de l'emplacement")
	}
}

// Sans gel : lecture directe sous data_root, aucun appel à Docker, avertissement.
func TestDockerNoPause(t *testing.T) {
	f := newDockerFixture(t)
	no := false
	f.cfg.Prepare[0].Pause, f.cfg.Prepare[0].Socket = &no, ""
	f.cfg.Prepare[0].DataRoot = f.fake.root
	d := f.deps()
	d.Docker = func(string) DockerAPI { t.Fatal("Docker appelé sans gel"); return nil }
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], d)
	if ps.Result != state.ResultWarning || !strings.Contains(ps.Message, "copie non figée") || len(ps.Paused) != 0 {
		t.Fatalf("%+v", ps)
	}
}

// Agent tué pendant le gel : l'exécution suivante dégèle ce qui est encore gelé.
func TestRecoverPaused(t *testing.T) {
	f := newDockerFixture(t)
	f.fake.set("aaa", "paused")
	if err := notePaused(Deps{PausedFile: f.paused, Now: time.Now}, f.cfg.Prepare[0], f.fake.containers[:2]); err != nil {
		t.Fatal(err)
	}
	thawed, err := RecoverPaused(context.Background(), f.paused, func(string) (DockerAPI, error) { return f.fake, nil })
	if err != nil || !slices.Equal(thawed, []string{"appli-db-1", "appli-sauvegarde"}) {
		t.Fatalf("%v %v", thawed, err)
	}
	if _, err := os.Stat(f.paused); !os.IsNotExist(err) {
		t.Fatal("fichier de reprise resté")
	}
	if thawed, err := RecoverPaused(context.Background(), f.paused, nil); err != nil || thawed != nil {
		t.Fatalf("sans fichier : %v %v", thawed, err)
	}
}

// Archive chiffrée par age pendant la lecture : jamais en clair dans la zone.
func TestDockerStreamedAge(t *testing.T) {
	f := newDockerFixture(t)
	id := newAgeIdentity(t)
	f.cfg.Prepare = append(f.cfg.Prepare, config.Prepare{ID: "appli-chiffre", Type: config.PrepareAge, Input: "appli",
		Recipients: []config.Recipient{{Age: id.Recipient().String()}}})
	f.cfg.ApplyDefaults()
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[1], f.deps())
	if ps.Result != state.ResultWarning || ps.Files != 1 {
		t.Fatalf("%+v", ps)
	}
	out := filepath.Join(f.staging, "appli-chiffre")
	if got := listTree(t, out); !slices.Equal(got, []string{"appli_db.tar.gz.age"}) {
		t.Fatalf("%v", got)
	}
	clear := filepath.Join(t.TempDir(), "x.tar.gz")
	os.WriteFile(clear, []byte(decryptAge(t, filepath.Join(out, "appli_db.tar.gz.age"), id)), 0o600)
	if hdrs, _ := tarEntries(t, clear); hdrs["base/PG_VERSION"] == nil {
		t.Fatal("archive déchiffrée incomplète")
	}
}
