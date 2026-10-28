package runner

import (
	"archive/zip"
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/procs"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// fakeZip joue Info-ZIP ; il note ce que la zone de préparation contient quand aws
// l'envoie (vérifié par le faux aws).
type fakeZip struct {
	code  int
	calls int
}

func (f *fakeZip) RunSpec(_ context.Context, s execx.Spec) (int, error) {
	f.calls++
	out, err := os.Create(s.Args[len(s.Args)-2])
	if err != nil {
		return -1, err
	}
	defer out.Close()
	w := zip.NewWriter(out)
	sc := bufio.NewScanner(s.Stdin)
	for sc.Scan() {
		e, _ := w.Create(sc.Text())
		e.Write([]byte("x"))
	}
	return f.code, w.Close()
}

// withPrepare ajoute une préparation zip du dossier Outlook et le job qui l'envoie.
func withPrepare(b *bench, dependsOnPreTask string) *fakeZip {
	z := &fakeZip{}
	b.zip = z
	b.cfg.Tools.Zip = "/usr/bin/zip"
	b.cfg.Prepare = []config.Prepare{{ID: "courrier", Type: config.PrepareZip, Source: b.outlook, DependsOnPreTask: dependsOnPreTask}}
	b.cfg.Jobs = append(b.cfg.Jobs, config.Job{ID: "courrier", Source: "${STAGING:courrier}", Destination: "COURRIER", Delete: true, Prepare: "courrier"})
	b.cfg.ApplyDefaults()
	b.cfg.ResolveStaging(filepath.Join(b.dir, "staging"))
	if err := b.cfg.Validate(config.ValidateOptions{}); err != nil {
		b.t.Fatal(err)
	}
	return z
}

func TestPrepareThenJob(t *testing.T) {
	b := newBench(t)
	z := withPrepare(b, "")
	staging := filepath.Join(b.dir, "staging")
	var sent []string
	b.aws.behave["COURRIER"] = func(dry bool, n int) (string, int) {
		entries, _ := os.ReadDir(filepath.Join(staging, "courrier"))
		for _, e := range entries {
			sent = append(sent, e.Name())
		}
		return "", 0
	}
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitOK || len(st.Prepares) != 1 || st.Prepares[0].Result != state.ResultOK || st.Prepares[0].Files != 1 {
		t.Fatalf("code %d, préparations %+v", st.ExitCode, st.Prepares)
	}
	if z.calls != 1 || b.aws.real("COURRIER") != 1 || len(sent) == 0 || sent[0] != "courrier.zip" {
		t.Fatalf("archive non envoyée : outil %d, aws %d, envoyé %v", z.calls, b.aws.real("COURRIER"), sent)
	}
	if left, _ := os.ReadDir(staging); len(left) != 0 {
		t.Errorf("zone de préparation non vidée : %v", left)
	}
}

// Une préparation en échec saute son job, sans rien envoyer ; les autres continuent.
func TestPrepareFailureSkipsItsJob(t *testing.T) {
	b := newBench(t)
	z := withPrepare(b, "")
	z.code = 2
	os.MkdirAll(filepath.Join(b.dir, "staging", "courrier"), 0o700) // reste d'une nuit précédente
	os.WriteFile(filepath.Join(b.dir, "staging", "courrier", "courrier.zip"), []byte("ancien"), 0o600)
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitError || st.Prepares[0].Result != state.ResultError {
		t.Fatalf("code %d, préparations %+v", st.ExitCode, st.Prepares)
	}
	if j := b.job(st, "courrier"); j.Result != state.ResultError || !strings.Contains(j.Message, "préparation courrier") {
		t.Fatalf("job : %+v", j)
	}
	if b.aws.real("COURRIER") != 0 || b.aws.dry("COURRIER") != 0 {
		t.Fatal("le job d'une préparation en échec ne doit rien envoyer")
	}
	if b.job(st, "data").Result != state.ResultOK {
		t.Error("les autres jobs doivent continuer")
	}
}

func TestPrepareNotRunInDryRunOrForOtherJobs(t *testing.T) {
	b := newBench(t)
	z := withPrepare(b, "")
	st := b.run(context.Background(), Options{DryRun: true})
	if z.calls != 0 || st.Prepares[0].Result != state.ResultOK || b.job(st, "courrier").Result != state.ResultOK {
		t.Fatalf("simulation : outil %d, %+v, %+v", z.calls, st.Prepares, b.job(st, "courrier"))
	}
	st = b.run(context.Background(), Options{JobIDs: []string{"data"}})
	if z.calls != 0 || len(st.Prepares) != 0 {
		t.Fatalf("préparation lancée pour un autre job : %d, %+v", z.calls, st.Prepares)
	}
}

// Une préparation qui attend la fermeture d'Outlook est sautée s'il reste ouvert.
func TestPrepareWaitsForPreTask(t *testing.T) {
	b := newBench(t)
	z := withPrepare(b, "outlook.exe")
	b.cfg.PreTasks[0].Force = false
	b.procs.running = []procs.Process{{PID: 7, Name: "outlook.exe", Session: 1}}
	b.procs.stubborn = true
	st := b.run(context.Background(), Options{JobIDs: []string{"courrier"}})
	if z.calls != 0 || st.Prepares[0].Result != state.ResultSkipped {
		t.Fatalf("préparation lancée malgré Outlook ouvert : %d, %+v", z.calls, st.Prepares)
	}
	if j := b.job(st, "courrier"); j.Result != state.ResultError {
		t.Fatalf("job : %+v", j)
	}
	if len(st.PreTasks) != 1 {
		t.Fatalf("la pré-tâche dont dépend la préparation doit tourner : %+v", st.PreTasks)
	}
}
