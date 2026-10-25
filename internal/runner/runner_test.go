package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/procs"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// --- fausse CLI AWS ---------------------------------------------------------

type call struct {
	dest string
	dry  bool
}

// behaviour renvoie la sortie et le code de sortie d'un appel ; n = rang de l'appel (0, 1, …)
// pour cette destination et ce mode.
type behaviour func(dry bool, n int) (string, int)

type fakeAWS struct {
	mu     sync.Mutex
	calls  []call
	behave map[string]behaviour
	counts map[call]int
}

func newFakeAWS() *fakeAWS {
	return &fakeAWS{behave: map[string]behaviour{}, counts: map[call]int{}}
}

func (f *fakeAWS) Run(ctx context.Context, name string, args, env []string, out io.Writer) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	dest := strings.TrimPrefix(args[3], "s3://acme-srv01/")
	c := call{dest: dest, dry: slices.Contains(args, "--dryrun")}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	n := f.counts[c]
	f.counts[c]++
	b := f.behave[dest]
	f.mu.Unlock()
	if b == nil {
		return 0, nil
	}
	text, code := b(c.dry, n)
	io.WriteString(out, text)
	return code, nil
}

func (f *fakeAWS) real(dest string) int { return f.counts[call{dest, false}] }
func (f *fakeAWS) dry(dest string) int  { return f.counts[call{dest, true}] }

// --- faux gestionnaire de processus -----------------------------------------

type fakeProcs struct {
	mu       sync.Mutex
	running  []procs.Process
	stubborn bool // Terminate échoue
	killed   []int
}

func (f *fakeProcs) Find(name string) ([]procs.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []procs.Process
	for _, p := range f.running {
		if strings.EqualFold(p.Name, name) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeProcs) Terminate(p procs.Process) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stubborn {
		return os.ErrPermission
	}
	f.killed = append(f.killed, p.PID)
	f.running = slices.DeleteFunc(f.running, func(q procs.Process) bool { return q.PID == p.PID })
	return nil
}

// --- banc d'essai -----------------------------------------------------------

type bench struct {
	t       *testing.T
	cfg     *config.Config
	aws     *fakeAWS
	procs   *fakeProcs
	data    string
	outlook string
	dir     string
	sleeps  []time.Duration
}

func newBench(t *testing.T) *bench {
	t.Helper()
	b := &bench{t: t, dir: t.TempDir(), aws: newFakeAWS(), procs: &fakeProcs{}}
	b.data = filepath.Join(b.dir, "E")
	b.outlook = filepath.Join(b.dir, "Users", "alice", "Outlook")
	for i := 0; i < 1200; i++ {
		mk(t, filepath.Join(b.data, "Compta", "f"+itoa(i)+".txt"))
	}
	mk(t, filepath.Join(b.outlook, "alice.pst"))
	b.cfg = &config.Config{
		Version:  1,
		Profile:  config.Profile{Name: "acme-srv01", Endpoint: "https://s3.eu-central-1.wasabisys.com", Bucket: "acme-srv01", ChecksumAlgorithm: "CRC32"},
		Schedule: config.Schedule{Type: "daily", Time: "22:30"},
		PreTasks: []config.PreTask{{Type: "close_process", Process: "outlook.exe", NoticeSeconds: 120, Force: true}},
		Jobs: []config.Job{
			{ID: "data", Source: b.data, Destination: "DATA", Delete: true,
				Exclude: []string{"*$RECYCLE.BIN*"},
				Guard:   &config.Guard{MinSourceFiles: 1000, MaxDeletes: 500, MaxDeleteRatio: 0.05}},
			{ID: "outlook", Source: b.outlook, Destination: "C/alice/Outlook", Delete: true, DependsOnPreTask: "outlook.exe"},
		},
	}
	b.cfg.ApplyDefaults()
	if err := b.cfg.Validate(config.ValidateOptions{}); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *bench) run(ctx context.Context, opt Options) state.Status {
	b.t.Helper()
	st, err := Run(ctx, b.cfg, opt, Deps{
		Exec: b.aws, Procs: b.procs, AWSCLI: "aws",
		StatusFile:  filepath.Join(b.dir, "status.json"),
		HistoryFile: filepath.Join(b.dir, "history.jsonl"),
		RunsDir:     filepath.Join(b.dir, "logs", "runs"),
		Sleep: func(ctx context.Context, d time.Duration) error {
			b.sleeps = append(b.sleeps, d)
			return ctx.Err()
		},
		RetryDelay: time.Minute,
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return st
}

func (b *bench) job(st state.Status, id string) state.JobStatus {
	for _, j := range st.Jobs {
		if j.ID == id {
			return j
		}
	}
	b.t.Fatalf("job %s absent du bilan", id)
	return state.JobStatus{}
}

func mk(t *testing.T, p string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// --- scénarios --------------------------------------------------------------

func TestNominalNight(t *testing.T) {
	b := newBench(t)
	b.procs.running = []procs.Process{{PID: 4242, Name: "OUTLOOK.EXE", Session: 2}}
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) {
		if dry {
			return "(dryrun) delete: s3://acme-srv01/DATA/old.txt\n(dryrun) upload: a to b\n", 0
		}
		return "upload: a to s3://acme-srv01/DATA/a\ndelete: s3://acme-srv01/DATA/old.txt\n", 0
	}
	st := b.run(context.Background(), Options{})

	if st.ExitCode != ExitOK || st.Result != state.ResultOK {
		t.Fatalf("nuit normale : code %d, résultat %s, %+v", st.ExitCode, st.Result, st)
	}
	if !slices.Equal(b.procs.killed, []int{4242}) {
		t.Errorf("Outlook (session 2) doit être arrêté, arrêtés : %v", b.procs.killed)
	}
	if len(b.sleeps) == 0 || b.sleeps[0] != 120*time.Second {
		t.Errorf("le préavis de 120 s doit précéder l'arrêt, attentes : %v", b.sleeps)
	}
	if p := st.PreTasks[0]; p.Found != 1 || p.Closed != 1 || !slices.Equal(p.Sessions, []int{2}) {
		t.Errorf("bilan de pré-tâche inattendu : %+v", p)
	}
	data := b.job(st, "data")
	if data.Uploaded != 1 || data.Deleted != 1 {
		t.Errorf("comptes du job data : %+v", data)
	}
	if b.aws.dry("DATA") != 1 || b.aws.real("DATA") != 1 {
		t.Errorf("data : 1 simulation puis 1 synchronisation attendues, appels %v", b.aws.calls)
	}
	if b.aws.dry("C/alice/Outlook") != 1 {
		t.Error("le job outlook (delete sans garde-fou configuré) doit recevoir le garde-fou par défaut")
	}
	if st.LastSuccess == nil {
		t.Error("last_success doit être renseigné après une nuit réussie")
	}

	// Fichiers produits
	saved, err := state.Read(filepath.Join(b.dir, "status.json"))
	if err != nil || saved == nil || saved.State != "idle" || saved.Result != state.ResultOK {
		t.Errorf("status.json final inattendu : %+v (%v)", saved, err)
	}
	hist, _ := os.ReadFile(filepath.Join(b.dir, "history.jsonl"))
	if strings.Count(string(hist), "\n") != 1 {
		t.Errorf("une ligne d'historique attendue, reçu :\n%s", hist)
	}
	for _, f := range []string{"run.log", "data.log", "outlook.log"} {
		if _, err := os.Stat(filepath.Join(b.dir, "logs", "runs", st.RunID, f)); err != nil {
			t.Errorf("journal %s manquant : %v", f, err)
		}
	}
	jl, _ := os.ReadFile(filepath.Join(b.dir, "logs", "runs", st.RunID, "data.log"))
	if !strings.Contains(string(jl), "--checksum-algorithm CRC32") || !strings.Contains(string(jl), "upload: a to") {
		t.Errorf("le journal du job doit contenir la commande et la sortie d'aws :\n%s", jl)
	}
}

func TestGuardBlocksWhenSourceLooksEmpty(t *testing.T) {
	b := newBench(t)
	os.RemoveAll(filepath.Join(b.data, "Compta")) // disque vidé ou mal remonté
	mk(t, filepath.Join(b.data, "reste.txt"))
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) {
		return strings.Repeat("(dryrun) delete: s3://acme-srv01/DATA/x\n", 1200), 0
	}
	st := b.run(context.Background(), Options{})

	if st.ExitCode != ExitGuard {
		t.Fatalf("code %d attendu (garde-fou), reçu %d", ExitGuard, st.ExitCode)
	}
	if b.aws.real("DATA") != 0 {
		t.Fatal("aucune synchronisation réelle ne doit toucher le bucket quand le garde-fou bloque")
	}
	if j := b.job(st, "data"); j.Result != state.ResultGuard || !strings.Contains(j.Message, "--force-deletes") {
		t.Errorf("bilan du job data : %+v", j)
	}
	if b.job(st, "outlook").Result != state.ResultOK {
		t.Error("un job bloqué ne doit pas empêcher les suivants")
	}
	if st.LastSuccess != nil {
		t.Error("une exécution bloquée n'est pas une sauvegarde réussie")
	}
}

func TestForceDeletesLiftsGuard(t *testing.T) {
	b := newBench(t)
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) {
		if dry {
			return strings.Repeat("(dryrun) delete: s3://acme-srv01/DATA/x\n", 600), 0
		}
		return "", 0
	}
	st := b.run(context.Background(), Options{ForceDeletes: true})
	if st.ExitCode != ExitOK || b.aws.real("DATA") != 1 {
		t.Fatalf("--force-deletes doit lever le garde-fou : code %d, appels %v", st.ExitCode, b.aws.calls)
	}
}

func TestLockedFilesGiveWarning(t *testing.T) {
	b := newBench(t)
	b.aws.behave["C/alice/Outlook"] = func(dry bool, n int) (string, int) {
		if dry {
			return "", 0
		}
		return "warning: Skipping file C:\\Users\\alice\\Outlook\\alice.pst. File/Directory is not readable.\n", 2
	}
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitWarning {
		t.Fatalf("code %d attendu, reçu %d", ExitWarning, st.ExitCode)
	}
	j := b.job(st, "outlook")
	if j.Result != state.ResultWarning || j.Skipped != 1 {
		t.Errorf("bilan outlook : %+v", j)
	}
	if st.LastSuccess == nil {
		t.Error("un avertissement compte comme sauvegarde réussie (pas d'alerte « 48 h sans succès »)")
	}
}

func TestTransientErrorIsRetried(t *testing.T) {
	b := newBench(t)
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) {
		if !dry && n == 0 {
			return "upload failed: connection reset\n", 1
		}
		return "", 0
	}
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitOK || b.aws.real("DATA") != 2 {
		t.Fatalf("une erreur passagère doit être retentée : code %d, appels réels %d", st.ExitCode, b.aws.real("DATA"))
	}
	if !slices.Contains(b.sleeps, time.Minute) {
		t.Errorf("le délai entre tentatives doit être respecté, attentes : %v", b.sleeps)
	}
}

func TestPersistentErrorAfterRetries(t *testing.T) {
	b := newBench(t)
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) {
		if dry {
			return "", 0
		}
		return "Could not connect to the endpoint URL\n", 255
	}
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitError {
		t.Fatalf("code %d attendu, reçu %d", ExitError, st.ExitCode)
	}
	if b.aws.real("DATA") != 3 {
		t.Errorf("1 essai + 2 nouvelles tentatives attendus, reçu %d", b.aws.real("DATA"))
	}
	if b.job(st, "outlook").Result != state.ResultOK {
		t.Error("les jobs suivants doivent être tentés malgré l'erreur")
	}
}

func TestConfigErrorIsNotRetried(t *testing.T) {
	b := newBench(t)
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) { return "", 252 }
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitError || b.aws.dry("DATA") != 1 {
		t.Fatalf("une erreur de syntaxe ne se retente pas : code %d, simulations %d", st.ExitCode, b.aws.dry("DATA"))
	}
}

func TestOutlookStillOpenSkipsDependentJob(t *testing.T) {
	b := newBench(t)
	b.procs.running = []procs.Process{{PID: 7, Name: "outlook.exe", Session: 3}}
	b.procs.stubborn = true
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitWarning {
		t.Fatalf("code %d attendu, reçu %d", ExitWarning, st.ExitCode)
	}
	if j := b.job(st, "outlook"); j.Result != state.ResultSkipped {
		t.Errorf("le job outlook doit être sauté : %+v", j)
	}
	if b.aws.real("C/alice/Outlook")+b.aws.dry("C/alice/Outlook") != 0 {
		t.Error("aucun appel aws pour un job sauté")
	}
	if b.job(st, "data").Result != state.ResultOK {
		t.Error("le job data ne dépend pas d'Outlook et doit passer")
	}
}

func TestMissingSourceIsAnErrorNotADeletion(t *testing.T) {
	b := newBench(t)
	os.RemoveAll(b.data) // E: démonté
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitError {
		t.Fatalf("code %d attendu, reçu %d", ExitError, st.ExitCode)
	}
	if b.aws.dry("DATA")+b.aws.real("DATA") != 0 {
		t.Fatal("aucun appel aws ne doit partir d'une source absente")
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	b := newBench(t)
	b.procs.running = []procs.Process{{PID: 9, Name: "outlook.exe"}}
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) {
		return "(dryrun) upload: a to b\n(dryrun) upload: c to d\n", 0
	}
	st := b.run(context.Background(), Options{DryRun: true})
	if st.ExitCode != ExitOK || !st.DryRun {
		t.Fatalf("simulation : code %d, %+v", st.ExitCode, st)
	}
	if len(b.procs.killed) != 0 {
		t.Error("une simulation ne ferme jamais Outlook")
	}
	if b.aws.real("DATA")+b.aws.real("C/alice/Outlook") != 0 {
		t.Error("une simulation ne lance aucune synchronisation réelle")
	}
	if b.aws.dry("DATA") != 1 {
		t.Errorf("la simulation du garde-fou doit servir aussi au bilan (1 seul appel), reçu %d", b.aws.dry("DATA"))
	}
	if j := b.job(st, "data"); j.Uploaded != 2 {
		t.Errorf("la simulation doit annoncer les envois prévus : %+v", j)
	}
	if st.LastSuccess != nil {
		t.Error("une simulation n'est pas une sauvegarde")
	}
}

func TestCancelDuringNotice(t *testing.T) {
	b := newBench(t)
	b.procs.running = []procs.Process{{PID: 9, Name: "outlook.exe"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := b.run(ctx, Options{})
	if st.ExitCode != ExitCancelled || st.Result != state.ResultCancelled {
		t.Fatalf("code %d attendu, reçu %d (%s)", ExitCancelled, st.ExitCode, st.Result)
	}
	if len(b.procs.killed) != 0 || len(b.aws.calls) != 0 {
		t.Error("après annulation, ni arrêt d'Outlook ni appel aws")
	}
}

func TestJobFilterSkipsUnneededPreTask(t *testing.T) {
	b := newBench(t)
	b.procs.running = []procs.Process{{PID: 9, Name: "outlook.exe"}}
	st := b.run(context.Background(), Options{JobIDs: []string{"data"}})
	if st.ExitCode != ExitOK || len(st.Jobs) != 1 {
		t.Fatalf("seul le job data doit tourner : %+v", st)
	}
	if len(b.procs.killed) != 0 {
		t.Error("Outlook ne doit pas être fermé si aucun job qui en dépend n'est lancé")
	}
}

func TestUnknownJobIsRejected(t *testing.T) {
	b := newBench(t)
	_, err := Run(context.Background(), b.cfg, Options{JobIDs: []string{"inconnu"}}, Deps{
		Exec: b.aws, Procs: b.procs, RunsDir: b.dir, StatusFile: filepath.Join(b.dir, "s.json")})
	if err == nil || !strings.Contains(err.Error(), "data, outlook") {
		t.Fatalf("un job inconnu doit être refusé en listant les jobs : %v", err)
	}
}

func TestLastSuccessSurvivesAFailedNight(t *testing.T) {
	b := newBench(t)
	first := b.run(context.Background(), Options{})
	if first.LastSuccess == nil {
		t.Fatal("première nuit : succès attendu")
	}
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) { return "", 253 }
	second := b.run(context.Background(), Options{})
	if second.ExitCode != ExitError || second.LastSuccess == nil || !second.LastSuccess.Equal(*first.LastSuccess) {
		t.Fatalf("la date du dernier succès doit être conservée après un échec : %+v", second.LastSuccess)
	}
	if first.RunID == second.RunID {
		t.Error("deux exécutions dans la même minute doivent avoir des run_id distincts")
	}
}
