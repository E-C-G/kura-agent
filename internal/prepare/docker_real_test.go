//go:build linux

package prepare

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/docker"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Tests avec un vrai Docker Engine (Linux, root) : KURA_DOCKER_TEST=1. Images :
// KURA_DOCKER_WRITER (image puis commande : écrit sans cesse un compteur dans
// /data/compteur.txt) et
// KURA_DOCKER_PG (PostgreSQL officiel, gel puis restauration d'une vraie base).

func needDocker(t *testing.T, image string) string {
	t.Helper()
	if os.Getenv("KURA_DOCKER_TEST") != "1" {
		t.Skip("KURA_DOCKER_TEST=1 pour les tests avec Docker")
	}
	if os.Geteuid() != 0 {
		t.Fatal("tests Docker : à lancer en root (lecture des volumes)")
	}
	img := os.Getenv(image)
	if img == "" {
		t.Skipf("%s non défini", image)
	}
	return img
}

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s : %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// testVolume crée un volume (et l'efface à la fin, avec ses conteneurs).
func testVolume(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	rand.Read(b)
	name := "kura-test-" + hex.EncodeToString(b)
	dockerCLI(t, "volume", "create", name)
	t.Cleanup(func() {
		exec.Command("sh", "-c", "docker rm -f $(docker ps -aq --filter volume="+name+") 2>/dev/null; docker volume rm -f "+name).Run()
	})
	return name
}

func containerState(t *testing.T, id string) string {
	return dockerCLI(t, "inspect", "-f", "{{.State.Status}}", id)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 120; i++ {
		if ok() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s : délai dépassé", what)
}

func realDockerRun(t *testing.T, p config.Prepare) (state.PrepareStatus, string, string) {
	t.Helper()
	cfg := &config.Config{Prepare: []config.Prepare{p}}
	cfg.ApplyDefaults()
	dir := t.TempDir()
	staging, paused := filepath.Join(dir, "staging"), filepath.Join(dir, "docker-paused.json")
	os.MkdirAll(staging, 0o700)
	ps := Run(context.Background(), cfg, cfg.Prepare[0], Deps{StagingDir: staging, PausedFile: paused})
	return ps, filepath.Join(staging, p.ID), paused
}

// counter : valeur du compteur dans l'archive du volume.
func counter(t *testing.T, archive string) int {
	t.Helper()
	_, data := tarEntries(t, archive)
	n, err := strconv.Atoi(data["compteur.txt"])
	if err != nil {
		t.Fatalf("compteur.txt dans l'archive : %q", data["compteur.txt"])
	}
	return n
}

// Conteneur qui écrit sans cesse : gelé pendant la lecture, dégelé après, il reprend.
func TestDockerRealFreeze(t *testing.T) {
	img := needDocker(t, "KURA_DOCKER_WRITER")
	vol := testVolume(t)
	id := dockerCLI(t, append([]string{"run", "-d", "-v", vol + ":/data"}, strings.Fields(img)...)...)
	mp := dockerCLI(t, "volume", "inspect", "-f", "{{.Mountpoint}}", vol)
	waitFor(t, "compteur", func() bool { _, err := os.Stat(filepath.Join(mp, "compteur.txt")); return err == nil })

	ps, out, paused := realDockerRun(t, config.Prepare{ID: "appli", Type: config.PrepareDocker, Volumes: []string{vol}})
	if ps.Result != state.ResultOK || len(ps.Paused) != 1 || ps.PauseS <= 0 {
		t.Fatalf("%+v", ps)
	}
	if st := containerState(t, id); st != "running" {
		t.Fatalf("conteneur %s après la préparation", st)
	}
	if _, err := os.Stat(paused); !os.IsNotExist(err) {
		t.Fatal("fichier de reprise resté")
	}
	n := counter(t, filepath.Join(out, vol+".tar.gz"))
	waitFor(t, "reprise de l'écriture", func() bool {
		b, _ := os.ReadFile(filepath.Join(mp, "compteur.txt"))
		v, err := strconv.Atoi(string(b))
		return err == nil && v > n+10
	})

	// Sans gel : rien n'est gelé, la copie est signalée non figée.
	no := false
	ps, _, _ = realDockerRun(t, config.Prepare{ID: "appli", Type: config.PrepareDocker, Volumes: []string{vol}, Pause: &no})
	if ps.Result != state.ResultWarning || len(ps.Paused) != 0 || !strings.Contains(ps.Message, "non figée") {
		t.Fatalf("sans gel : %+v", ps)
	}

	// Agent tué pendant le gel : la reprise dégèle.
	dockerCLI(t, "pause", id)
	file := filepath.Join(t.TempDir(), "docker-paused.json")
	d := Deps{PausedFile: file, Now: time.Now}
	if err := notePaused(d, config.Prepare{ID: "appli", Socket: config.DockerSocket}, []docker.Container{{ID: id, Names: []string{"/x"}}}); err != nil {
		t.Fatal(err)
	}
	if thawed, err := RecoverPaused(context.Background(), file, d.Client); err != nil || len(thawed) != 1 {
		t.Fatalf("reprise : %v %v", thawed, err)
	}
	if st := containerState(t, id); st != "running" {
		t.Fatalf("conteneur %s après la reprise", st)
	}
}

// Vraie base PostgreSQL écrite pendant la sauvegarde : l'archive, restaurée dans un
// volume neuf, redémarre la base avec toutes les lignes validées avant le gel.
func TestDockerRealPostgres(t *testing.T) {
	img := needDocker(t, "KURA_DOCKER_PG")
	vol := testVolume(t)
	id := dockerCLI(t, "run", "-d", "-e", "POSTGRES_PASSWORD=kura", "-v", vol+":/var/lib/postgresql/data", img)
	psql := func(id, sql string) (string, error) {
		out, err := exec.Command("docker", "exec", id, "psql", "-U", "postgres", "-tAc", sql).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	// Prête quand elle écoute en TCP : le serveur temporaire de l'initialisation n'écoute
	// que sur son socket.
	ready := func(id string) func() bool {
		return func() bool { return exec.Command("docker", "exec", id, "pg_isready", "-h", "127.0.0.1").Run() == nil }
	}
	waitFor(t, "démarrage de PostgreSQL", ready(id))
	if out, err := psql(id, "create table t(i int); insert into t select generate_series(1, 5000)"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	// Écritures en continu pendant la sauvegarde.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				psql(id, "insert into t values (0)")
			}
		}
	}()
	time.Sleep(time.Second)
	ps, out, _ := realDockerRun(t, config.Prepare{ID: "pg", Type: config.PrepareDocker, Volumes: []string{vol}})
	close(stop)
	<-done
	if ps.Result == state.ResultError || len(ps.Paused) != 1 {
		t.Fatalf("%+v", ps)
	}
	if st := containerState(t, id); st != "running" {
		t.Fatalf("conteneur %s après la préparation", st)
	}

	// Restauration : volume neuf, archive extraite avec propriétaires et droits.
	restored := testVolume(t)
	mp := dockerCLI(t, "volume", "inspect", "-f", "{{.Mountpoint}}", restored)
	if b, err := exec.Command("tar", "-xzpf", filepath.Join(out, vol+".tar.gz"), "--numeric-owner", "-C", mp).CombinedOutput(); err != nil {
		t.Fatalf("tar : %v %s", err, b)
	}
	id2 := dockerCLI(t, "run", "-d", "-v", restored+":/var/lib/postgresql/data", img)
	waitFor(t, "démarrage de la base restaurée", ready(id2))
	got, err := psql(id2, "select count(*) from t where i > 0")
	if err != nil || got != "5000" {
		t.Fatalf("base restaurée : %q %v\n%s", got, err, dockerCLI(t, "logs", id2))
	}
}
