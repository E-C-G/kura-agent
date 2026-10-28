package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/ipc"
	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/selfupdate"
)

// Banc de self-update : installation Windows factice dans un dossier temporaire ; les
// « exécutables » sont des fichiers texte dont exeVersion lit la première ligne.
type updateBench struct {
	t          *testing.T
	dir        string
	inst       installation
	u          *updater
	out        bytes.Buffer
	latest     string // version publiée
	newVersion string // version annoncée par l'exécutable de l'archive
	finishErr  error
	downloads  int
}

func newUpdateBench(t *testing.T, installed string) *updateBench {
	t.Helper()
	b := &updateBench{t: t, dir: t.TempDir(), latest: "0.10.0", newVersion: "0.10.0"}
	b.inst = installation{Mode: selfupdate.ModeFiles, Exe: filepath.Join(b.dir, "kura-agent.exe"),
		Staging: filepath.Join(b.dir, ".update")}
	gui := filepath.Join(b.dir, "kura-agentw.exe")
	b.inst.Files = []string{b.inst.Exe, gui}
	writeText(t, b.inst.Exe, "kura-agent "+installed+" (installé)\n")
	writeText(t, gui, "kura-agent "+installed+" (icône)\n")
	b.u = &updater{
		out:         &b.out,
		checkAdmin:  func() error { return nil },
		locate:      func() (installation, error) { return b.inst, nil },
		acquireLock: func() (lock.Release, error) { return func() {}, nil },
		statusFile:  filepath.Join(b.dir, "update.json"),
		logsDir:     filepath.Join(b.dir, "logs"),
		exeVersion: func(exe string) (string, error) {
			data, err := os.ReadFile(exe)
			if err != nil {
				return "", err
			}
			m := versionLine.FindStringSubmatch(string(data))
			if m == nil {
				return "", errors.New("illisible")
			}
			return m[1], nil
		},
		finish:      func(string) error { return b.finishErr },
		dpkgInstall: func(string) error { return errors.New("pas de paquet ici") },
		debExtract:  func(string, string) (string, error) { return "", errors.New("pas de paquet ici") },
		freeSpace:   func(string) (uint64, error) { return 1 << 40, nil },
		interactive: false,
		confirm:     func(string) (bool, error) { return true, nil },
		goos:        "windows",
		goarch:      "amd64",
		now:         func() time.Time { return time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC) },
	}
	b.u.check = func(_ context.Context, o selfupdate.Options) (selfupdate.Result, error) {
		v := b.latest
		if o.Tag != "" {
			v = strings.TrimPrefix(o.Tag, "v")
		}
		cmp, _ := selfupdate.Compare(v, o.Current)
		return selfupdate.Result{Current: o.Current, Newer: cmp > 0, Verified: true,
			Latest: selfupdate.Release{Tag: "v" + v, Version: v, NotesURL: "https://github.com/E-C-G/kura-agent/releases/tag/v" + v},
			Asset:  selfupdate.Asset{Name: "kura-agent_" + v + "_windows_amd64.zip", Size: 1000}}, nil
	}
	b.u.download = func(_ context.Context, a selfupdate.Asset, dst string) error {
		b.downloads++
		v := strings.TrimSuffix(strings.TrimPrefix(a.Name, "kura-agent_"), "_windows_amd64.zip")
		announced := b.newVersion
		if v != b.latest {
			announced = v // retour arrière vers une version publiée
		}
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, n := range []string{"kura-agent.exe", "kura-agentw.exe"} {
			w, _ := zw.Create("kura-agent_" + v + "_windows_amd64/" + n)
			w.Write([]byte("kura-agent " + announced + " (nouveau " + n + ")\n"))
		}
		zw.Close()
		return os.WriteFile(dst, buf.Bytes(), 0o600)
	}
	return b
}

func writeText(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (b *updateBench) run(o updateOptions) int {
	b.t.Helper()
	b.out.Reset()
	return b.u.run(context.Background(), o)
}

func (b *updateBench) installed() string {
	v, err := b.u.exeVersion(b.inst.Exe)
	if err != nil {
		b.t.Fatal(err)
	}
	return v
}

func (b *updateBench) status() selfupdate.Status {
	s, err := selfupdate.ReadStatus(b.u.statusFile)
	if err != nil {
		b.t.Fatal(err)
	}
	return s
}

func TestSelfUpdateInstallsAndRollsBack(t *testing.T) {
	b := newUpdateBench(t, "0.9.0")
	if code := b.run(updateOptions{yes: true}); code != exitUpdateOK {
		t.Fatalf("code %d\n%s", code, b.out.String())
	}
	if b.installed() != "0.10.0" {
		t.Fatalf("version installée %s", b.installed())
	}
	gui, _ := b.u.exeVersion(b.inst.Files[1])
	if gui != "0.10.0" || !strings.Contains(b.out.String(), "prochaine ouverture de session") {
		t.Fatalf("icône : %s\n%s", gui, b.out.String())
	}
	st := b.status()
	if st.Journal != nil || st.Last == nil || st.Last.Result != "ok" || st.Last.From != "0.9.0" || st.Last.To != "0.10.0" {
		t.Fatalf("état : %+v", st)
	}
	if _, err := os.Stat(b.inst.Staging); err == nil {
		t.Error("dossier de travail resté")
	}
	if logs, _ := filepath.Glob(filepath.Join(b.u.logsDir, "update-*.log")); len(logs) != 1 {
		t.Errorf("journal de mise à jour : %v", logs)
	}

	// Déjà à jour : rien ne change, rien n'est téléchargé.
	n := b.downloads
	if code := b.run(updateOptions{yes: true}); code != exitUpdateOK || b.downloads != n || !strings.Contains(b.out.String(), "déjà à jour") {
		t.Fatalf("déjà à jour : %d\n%s", code, b.out.String())
	}

	// Retour à la version précédente, une seule fois.
	if code := b.run(updateOptions{rollback: true}); code != exitUpdateOK || b.installed() != "0.9.0" {
		t.Fatalf("rollback : %d %s\n%s", code, b.installed(), b.out.String())
	}
	if st := b.status(); st.Last.Result != "rollback" || st.Last.From != "0.10.0" || st.Last.To != "0.9.0" {
		t.Fatalf("état après retour : %+v", st.Last)
	}
	if code := b.run(updateOptions{rollback: true}); code != exitUpdateRefused {
		t.Fatalf("second rollback : %d\n%s", code, b.out.String())
	}
}

// La nouvelle version refuse la configuration : l'ancienne est remise (code 6).
func TestSelfUpdateFinishFailureRestores(t *testing.T) {
	b := newUpdateBench(t, "0.9.0")
	b.finishErr = errors.New("configuration installée refusée")
	if code := b.run(updateOptions{yes: true}); code != exitUpdateRolledBack {
		t.Fatalf("code %d\n%s", code, b.out.String())
	}
	if b.installed() != "0.9.0" {
		t.Fatalf("version %s", b.installed())
	}
	if st := b.status(); st.Journal != nil || st.Last.Result != "rolled_back" || !strings.Contains(st.Last.Message, "refusée") {
		t.Fatalf("état : %+v", st)
	}
}

// Retour arrière impossible (version précédente disparue) : code 7, consigne claire.
func TestSelfUpdateBrokenRollback(t *testing.T) {
	b := newUpdateBench(t, "0.9.0")
	b.u.finish = func(string) error {
		for _, f := range b.inst.Files {
			os.Remove(f + selfupdate.BackupSuffix)
		}
		return errors.New("démarrage impossible")
	}
	if code := b.run(updateOptions{yes: true}); code != exitUpdateBroken || !strings.Contains(b.out.String(), "Réinstaller") {
		t.Fatalf("code %d\n%s", code, b.out.String())
	}
}

func TestSelfUpdateRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(b *updateBench)
		o     updateOptions
		code  int
		want  string
	}{
		{"droits", func(b *updateBench) { b.u.checkAdmin = func() error { return errors.New("relancer avec sudo") } }, updateOptions{yes: true}, exitUpdateRefused, "sudo"},
		{"sauvegarde en cours", func(b *updateBench) {
			b.u.acquireLock = func() (lock.Release, error) { return nil, lock.ErrBusy }
		}, updateOptions{yes: true}, exitUpdateRefused, "sauvegarde est en cours"},
		{"sans terminal", func(b *updateBench) {}, updateOptions{}, exitUpdateRefused, "--yes"},
		{"non confirmé", func(b *updateBench) {
			b.u.interactive = true
			b.u.confirm = func(string) (bool, error) { return false, nil }
		}, updateOptions{}, exitUpdateRefused, "non confirmée"},
		{"plus ancienne", func(b *updateBench) {}, updateOptions{yes: true, tag: "v0.9.5"}, exitUpdateRefused, "0.10.0 au moins"},
		{"inférieure à l'installée", func(b *updateBench) {
			writeText(b.t, b.inst.Exe, "kura-agent 0.11.0 (installé)\n")
		}, updateOptions{yes: true, tag: "v0.10.0"}, exitUpdateRefused, "plus ancienne"},
		{"disque", func(b *updateBench) { b.u.freeSpace = func(string) (uint64, error) { return 100, nil } }, updateOptions{yes: true}, exitUpdateRefused, "espace disque"},
		{"version annoncée", func(b *updateBench) { b.newVersion = "0.9.9" }, updateOptions{yes: true}, exitUpdateFailed, "annonce la version 0.9.9"},
		{"recherche", func(b *updateBench) {
			b.u.check = func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
				return selfupdate.Result{}, errors.New("attestation invalide")
			}
		}, updateOptions{yes: true}, exitUpdateFailed, "attestation invalide"},
		{"développement", func(b *updateBench) { writeText(b.t, b.inst.Exe, "kura-agent dev (commit none)\n") }, updateOptions{yes: true}, exitUpdateRefused, "développement"},
	}
	for _, c := range cases {
		b := newUpdateBench(t, "0.9.0")
		c.setup(b)
		before, _ := os.ReadFile(b.inst.Exe)
		code := b.run(c.o)
		after, _ := os.ReadFile(b.inst.Exe)
		if code != c.code || !strings.Contains(b.out.String(), c.want) || !bytes.Equal(before, after) {
			t.Errorf("%s : code %d, exécutable modifié=%v\n%s", c.name, code, !bytes.Equal(before, after), b.out.String())
		}
		if _, err := os.Stat(b.inst.Exe + selfupdate.BackupSuffix); err == nil {
			t.Errorf("%s : sauvegarde créée sans remplacement", c.name)
		}
	}
}

// Coupure pendant le remplacement : la commande suivante remet la version précédente.
func TestSelfUpdateRecoversInterruptedUpdate(t *testing.T) {
	b := newUpdateBench(t, "0.9.0")
	// État après la coupure : kura-agent.exe déjà mis de côté, le nouveau pas encore en place.
	os.Rename(b.inst.Exe, b.inst.Exe+selfupdate.BackupSuffix)
	writeText(t, b.inst.Exe, "kura-agent 0.10.0 (à moitié)\n")
	st := selfupdate.Status{Journal: &selfupdate.Journal{From: "0.9.0", To: "0.10.0", Mode: selfupdate.ModeFiles, Step: "replacing"}}
	b.u.writeStatus(st)
	b.u.recover(b.inst, &b.out)
	if b.installed() != "0.9.0" || !strings.Contains(b.out.String(), "remise") {
		t.Fatalf("%s\n%s", b.installed(), b.out.String())
	}
	if s := b.status(); s.Journal != nil || s.Last == nil || s.Last.Result != "failed" {
		t.Fatalf("état : %+v", s)
	}
}

func TestFinishUpdate(t *testing.T) {
	var out, errb bytes.Buffer
	dir := t.TempDir()
	if code := finishUpdate(&out, &errb, filepath.Join(dir, "absent.yaml")); code != 0 {
		t.Fatalf("sans configuration : %d %s", code, errb.String())
	}
	bad := filepath.Join(dir, "config.yaml")
	writeText(t, bad, "version: 1\noption_inconnue: oui\n")
	if code := finishUpdate(&out, &errb, bad); code != 1 || !strings.Contains(errb.String(), "refusée") {
		t.Fatalf("configuration refusée : %d %s", code, errb.String())
	}
}

// fakeTrays enregistre ce que self-update annonce aux icônes.
type fakeTrays struct {
	msgs   []ipc.Msg
	closed bool
}

func (f *fakeTrays) Broadcast(m ipc.Msg) { f.msgs = append(f.msgs, m) }
func (f *fakeTrays) Close()              { f.closed = true }

func (f *fakeTrays) types() string {
	var t []string
	for _, m := range f.msgs {
		t = append(t, m.Type)
	}
	return strings.Join(t, ",")
}

func TestSelfUpdateNotifiesTrays(t *testing.T) {
	b := newUpdateBench(t, "0.9.0")
	tr := &fakeTrays{}
	b.u.trays = tr
	if code := b.u.runAndNotify(context.Background(), updateOptions{yes: true}); code != exitUpdateOK {
		t.Fatalf("code %d\n%s", code, b.out.String())
	}
	want := "update_progress,update_progress,update_progress,update_progress,update_done,restart"
	if tr.types() != want || !tr.closed {
		t.Fatalf("messages : %s", tr.types())
	}
	done := tr.msgs[len(tr.msgs)-2]
	if !done.OK || !strings.Contains(done.Message, "0.9.0 → 0.10.0") {
		t.Fatalf("fin : %+v", done)
	}

	// Déjà à jour : fin annoncée, pas de relance.
	b.u.changed = false
	tr2 := &fakeTrays{}
	b.u.trays = tr2
	if code := b.u.runAndNotify(context.Background(), updateOptions{yes: true}); code != exitUpdateOK || strings.Contains(tr2.types(), "restart") {
		t.Fatalf("déjà à jour : %d %s", code, tr2.types())
	}

	// Refus (sauvegarde en cours) : fin en échec, avec la raison, pas de relance.
	b2 := newUpdateBench(t, "0.9.0")
	b2.u.acquireLock = func() (lock.Release, error) { return nil, lock.ErrBusy }
	tr3 := &fakeTrays{}
	b2.u.trays = tr3
	b2.u.runAndNotify(context.Background(), updateOptions{yes: true})
	if tr3.types() != "update_done" || tr3.msgs[0].OK || !strings.Contains(tr3.msgs[0].Message, "sauvegarde est en cours") {
		t.Fatalf("refus : %+v", tr3.msgs)
	}
}
