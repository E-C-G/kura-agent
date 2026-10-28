package runner

import (
	"archive/zip"
	"bufio"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

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
	// La taille du résultat est gardée d'une exécution à l'autre (espace libre de la
	// suivante), même par une exécution qui ne fait pas la préparation.
	if st.PrepareLastSize["courrier"] != st.Prepares[0].Size || st.Prepares[0].Size == 0 {
		t.Fatalf("taille gardée : %v", st.PrepareLastSize)
	}
	st = b.run(context.Background(), Options{JobIDs: []string{"data"}})
	if st.PrepareLastSize["courrier"] == 0 {
		t.Fatalf("taille perdue : %v", st.PrepareLastSize)
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
	if j := b.job(st, "courrier"); j.Result != state.ResultSkipped {
		t.Fatalf("job : %+v", j)
	}
	if len(st.PreTasks) != 1 {
		t.Fatalf("la pré-tâche dont dépend la préparation doit tourner : %+v", st.PreTasks)
	}
}

// Les restes d'une exécution interrompue sont effacés, même quand aucune préparation ne
// tourne ; le résultat d'une préparation disparaît après le dernier job qui l'envoie.
func TestStagingCleanup(t *testing.T) {
	b := newBench(t)
	withPrepare(b, "")
	staging := filepath.Join(b.dir, "staging")
	os.MkdirAll(filepath.Join(staging, "ancien"), 0o700)
	os.WriteFile(filepath.Join(staging, "ancien", "export.dump"), []byte("x"), 0o600)
	b.run(context.Background(), Options{JobIDs: []string{"data"}})
	if left, _ := os.ReadDir(staging); len(left) != 0 {
		t.Fatalf("restes non effacés : %v", left)
	}

	b.cfg.Jobs = append(b.cfg.Jobs, config.Job{ID: "copie", Source: filepath.Join(staging, "courrier"), Destination: "COPIE", Prepare: "courrier"})
	seen := map[string]bool{}
	for _, dest := range []string{"COURRIER", "COPIE"} {
		b.aws.behave[dest] = func(bool, int) (string, int) {
			_, err := os.Stat(filepath.Join(staging, "courrier", "courrier.zip"))
			seen[dest] = err == nil
			return "", 0
		}
	}
	b.aws.behave["DATA"] = func(bool, int) (string, int) {
		_, err := os.Stat(filepath.Join(staging, "courrier"))
		seen["DATA après"] = err == nil
		return "", 0
	}
	b.cfg.Jobs[0], b.cfg.Jobs[len(b.cfg.Jobs)-1] = b.cfg.Jobs[len(b.cfg.Jobs)-1], b.cfg.Jobs[0] // data en dernier
	st := b.run(context.Background(), Options{JobIDs: []string{"courrier", "copie", "data"}})
	if !seen["COURRIER"] || !seen["COPIE"] || seen["DATA après"] {
		t.Fatalf("archive vue : %v (%+v)", seen, st.Jobs)
	}
}

// Une archive chiffrée : le zip tourne d'abord, son clair est chiffré puis effacé, seul
// le chiffré part au bucket.
func TestEncryptedPrepare(t *testing.T) {
	b := newBench(t)
	withPrepare(b, "")
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	b.cfg.Prepare = append(b.cfg.Prepare, config.Prepare{ID: "courrier-chiffre", Type: config.PrepareAge, Input: "courrier",
		Recipients: []config.Recipient{{Age: id.Recipient().String()}}})
	j := &b.cfg.Jobs[len(b.cfg.Jobs)-1]
	j.Source, j.Prepare = "${STAGING:courrier-chiffre}", "courrier-chiffre"
	b.cfg.ApplyDefaults()
	staging := filepath.Join(b.dir, "staging")
	b.cfg.ResolveStaging(staging)
	if err := b.cfg.Validate(config.ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	var sent []string
	b.aws.behave["COURRIER"] = func(dry bool, n int) (string, int) {
		if !dry {
			sent = listDir(staging)
		}
		return "", 0
	}
	st := b.run(context.Background(), Options{JobIDs: []string{"courrier"}})
	if st.ExitCode != ExitOK || len(st.Prepares) != 2 || st.Prepares[0].ID != "courrier" || st.Prepares[1].ID != "courrier-chiffre" {
		t.Fatalf("code %d, préparations %+v", st.ExitCode, st.Prepares)
	}
	if !slices.Equal(sent, []string{"courrier-chiffre/courrier.zip.age"}) {
		t.Fatalf("zone pendant l'envoi : %v", sent)
	}
}

// Préparation chiffrée dont l'entrée échoue : sautée en erreur, rien n'est envoyé.
func TestEncryptedPrepareInputFails(t *testing.T) {
	b := newBench(t)
	z := withPrepare(b, "")
	z.code = 2
	id, _ := age.GenerateX25519Identity()
	b.cfg.Prepare = append(b.cfg.Prepare, config.Prepare{ID: "courrier-chiffre", Type: config.PrepareAge, Input: "courrier",
		Recipients: []config.Recipient{{Age: id.Recipient().String()}}})
	j := &b.cfg.Jobs[len(b.cfg.Jobs)-1]
	j.Source, j.Prepare = "${STAGING:courrier-chiffre}", "courrier-chiffre"
	b.cfg.ApplyDefaults()
	st := b.run(context.Background(), Options{JobIDs: []string{"courrier"}})
	if st.ExitCode != ExitError || len(st.Prepares) != 2 || !strings.Contains(st.Prepares[1].Message, "préparation courrier en échec") {
		t.Fatalf("code %d, préparations %+v", st.ExitCode, st.Prepares)
	}
	if b.aws.real("COURRIER") != 0 {
		t.Fatal("rien ne doit partir")
	}
}

// L'export chiffré par age au fil de l'eau ne tourne pas à part.
func TestStreamedExportNotRunAlone(t *testing.T) {
	cfg := &config.Config{Prepare: []config.Prepare{
		{ID: "compta-db", Type: config.PrepareDBExport, DependsOnPreTask: "compta.exe"},
		{ID: "compta-chiffre", Type: config.PrepareAge, Input: "compta-db"},
		{ID: "courrier", Type: config.PrepareZip},
		{ID: "courrier-chiffre", Type: config.PrepareGPG, Input: "courrier"},
	}}
	r := &run{cfg: cfg}
	jobs := []config.Job{{ID: "a", Prepare: "compta-chiffre"}, {ID: "b", Prepare: "courrier-chiffre"}}
	if got := r.neededPrepares(jobs); !slices.Equal(got, []string{"compta-chiffre", "courrier", "courrier-chiffre"}) {
		t.Fatalf("%v", got)
	}
	if dep := r.prepareDep(cfg.Prepare[1]); dep != "compta.exe" {
		t.Fatalf("pré-tâche de l'entrée : %q", dep)
	}
	if !r.waitsForPreTask(jobs[0]) || r.waitsForPreTask(jobs[1]) {
		t.Fatal("attente de la pré-tâche")
	}
}

func listDir(dir string) []string {
	var out []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}
