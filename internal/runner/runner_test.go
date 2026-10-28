package runner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	envs   [][]string
	args   map[call][][]string
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
	f.envs = append(f.envs, env)
	if f.args == nil {
		f.args = map[call][][]string{}
	}
	f.args[c] = append(f.args[c], args)
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
	t           *testing.T
	cfg         *config.Config
	aws         *fakeAWS
	procs       *fakeProcs
	data        string
	outlook     string
	dir         string
	sleeps      []time.Duration
	onSleep     func() // appelé à chaque attente (préavis, délai entre tentatives)
	trays       Trays
	smu         sync.Mutex
	quarantined []string
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
			b.smu.Lock()
			b.sleeps = append(b.sleeps, d)
			b.smu.Unlock()
			if b.onSleep != nil {
				b.onSleep()
			}
			return ctx.Err()
		},
		RetryDelay: time.Minute,
		Trays:      b.trays,
		Quarantine: func(p []string) error {
			b.smu.Lock()
			b.quarantined = append(b.quarantined, p...)
			b.smu.Unlock()
			return nil
		},
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
	if !strings.Contains(string(jl), "s3 sync") || !strings.Contains(string(jl), "upload: a to") {
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
	if j := b.job(st, "data"); j.Result != state.ResultGuard || !strings.Contains(j.Message, "source vide") {
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

// Liens cassés dans un dossier exclu (.venv créé sous WSL) : la CLI les signale et
// sort en code 2, mais rien de ce qui devait être sauvegardé n'a manqué.
func TestSkippedExcludedFilesAreNotAWarning(t *testing.T) {
	b := newBench(t)
	b.cfg.Jobs[0].Exclude = append(b.cfg.Jobs[0].Exclude, "*/.venv/*")
	skip := "warning: Skipping file " + filepath.Join(b.data, "outil", ".venv", "lib64") + ". File does not exist.\n"
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) { return skip, 2 }
	st := b.run(context.Background(), Options{})
	j := b.job(st, "data")
	if st.ExitCode != ExitOK || j.Result != state.ResultOK || j.Skipped != 0 || !strings.Contains(j.Message, "exclu") {
		t.Fatalf("code %d, job %+v", st.ExitCode, j)
	}
	// Un fichier non exclu reste un avertissement.
	b = newBench(t)
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) {
		return "warning: Skipping file " + filepath.Join(b.data, "Compta", "verrou.xlsx") + ". File/Directory is not readable.\n", 2
	}
	if st := b.run(context.Background(), Options{}); st.ExitCode != ExitWarning {
		t.Fatalf("fichier verrouillé : code %d", st.ExitCode)
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

// Une simulation qui réussit à la seconde tentative (erreur réseau passagère) ne
// laisse pas le job, ni l'exécution, en erreur.
func TestDryRunRetrySucceeds(t *testing.T) {
	b := newBench(t)
	flaky := func(dry bool, n int) (string, int) {
		if n == 0 {
			return "fatal error: Could not connect to the endpoint URL\n", 1
		}
		return "(dryrun) upload: a to b\n", 0
	}
	b.aws.behave["DATA"] = flaky
	b.aws.behave["C/alice/Outlook"] = flaky
	st := b.run(context.Background(), Options{DryRun: true})
	if st.ExitCode != ExitOK || st.Result != state.ResultOK {
		t.Fatalf("simulation réussie à la seconde tentative : code %d, %s", st.ExitCode, st.Result)
	}
	for _, id := range []string{"data", "outlook"} {
		if j := b.job(st, id); j.Result != state.ResultOK || j.Uploaded != 1 || j.Attempts != 2 || !strings.HasPrefix(j.Message, "simulation :") {
			t.Errorf("job %s : %+v", id, j)
		}
	}
	if b.aws.dry("DATA") != 2 || b.aws.dry("C/alice/Outlook") != 2 {
		t.Errorf("appels de simulation : %d %d", b.aws.dry("DATA"), b.aws.dry("C/alice/Outlook"))
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

// --- défauts relevés en relecture ------------------------------------------

// Entre deux tentatives, le disque est démonté : la nouvelle tentative ne doit pas
// partir avec --delete sur une source absente.
func TestGuardIsCheckedAgainBeforeEachRetry(t *testing.T) {
	b := newBench(t)
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) {
		if !dry && n == 0 {
			os.RemoveAll(b.data) // le volume disparaît pendant l'échec réseau
			return "upload failed: connection reset\n", 1
		}
		return "", 0
	}
	st := b.run(context.Background(), Options{})
	if b.aws.real("DATA") != 1 {
		t.Fatalf("aucune nouvelle synchronisation ne doit partir sur une source disparue, appels réels : %d", b.aws.real("DATA"))
	}
	if j := b.job(st, "data"); j.Result != state.ResultError || !strings.Contains(j.Message, "inaccessible") {
		t.Errorf("bilan du job data : %+v", j)
	}
}

// Le disque est vidé entre deux tentatives : le garde-fou doit le voir à la seconde.
func TestGuardBlocksRetryWhenSourceEmptiedMeanwhile(t *testing.T) {
	b := newBench(t)
	b.aws.behave["DATA"] = func(dry bool, n int) (string, int) {
		if !dry && n == 0 {
			os.RemoveAll(filepath.Join(b.data, "Compta"))
			return "", 255
		}
		if dry && n > 0 {
			return strings.Repeat("(dryrun) delete: s3://acme-srv01/DATA/x\n", 1200), 0
		}
		return "", 0
	}
	st := b.run(context.Background(), Options{})
	if b.aws.real("DATA") != 1 || st.ExitCode != ExitGuard {
		t.Fatalf("la seconde tentative doit être bloquée par le garde-fou : code %d, appels réels %d", st.ExitCode, b.aws.real("DATA"))
	}
}

// Un garde-fou partiel garde le minimum d'un fichier : un dossier vide ne vide pas le bucket.
func TestPartialGuardKeepsMinimumOneFile(t *testing.T) {
	b := newBench(t)
	b.cfg.Jobs[1].Guard = &config.Guard{MaxDeletes: 500}
	b.cfg.ApplyDefaults()
	os.Remove(filepath.Join(b.outlook, "alice.pst"))
	b.aws.behave["C/alice/Outlook"] = func(dry bool, n int) (string, int) {
		return "(dryrun) delete: s3://acme-srv01/C/alice/Outlook/alice.pst\n", 0
	}
	st := b.run(context.Background(), Options{})
	if b.aws.real("C/alice/Outlook") != 0 || b.job(st, "outlook").Result != state.ResultGuard {
		t.Fatalf("un dossier Outlook vide doit être bloqué : %+v", b.job(st, "outlook"))
	}
}

// --force-deletes lève les plafonds, jamais le minimum de fichiers.
func TestForceDeletesNeverForcesAnEmptySource(t *testing.T) {
	b := newBench(t)
	os.RemoveAll(filepath.Join(b.data, "Compta"))
	mk(t, filepath.Join(b.data, "seul.txt"))
	st := b.run(context.Background(), Options{JobIDs: []string{"data"}, ForceDeletes: true})
	if b.aws.real("DATA") != 0 || st.ExitCode != ExitGuard {
		t.Fatalf("une source quasi vide ne se force pas : code %d, appels réels %d", st.ExitCode, b.aws.real("DATA"))
	}
	if strings.Contains(b.job(st, "data").Message, "--force-deletes") {
		t.Error("le message ne doit pas suggérer --force-deletes pour une source vide")
	}
}

// Outlook est fermé par l'utilisateur pendant le préavis : on ne tue rien
// (le PID mémorisé a pu être réattribué).
func TestNoKillWhenProcessClosedDuringNotice(t *testing.T) {
	b := newBench(t)
	b.procs.running = []procs.Process{{PID: 4242, Name: "outlook.exe", Session: 2}}
	b.onSleep = func() { b.procs.running = nil }
	st := b.run(context.Background(), Options{})
	if len(b.procs.killed) != 0 {
		t.Fatalf("aucun arrêt attendu, arrêtés : %v", b.procs.killed)
	}
	if st.ExitCode != ExitOK || b.job(st, "outlook").Result != state.ResultOK {
		t.Errorf("Outlook fermé par l'utilisateur : nuit normale attendue, %+v", st)
	}
}

// Un nouveau processus apparu pendant le préavis est celui qu'on arrête,
// pas l'ancien PID.
func TestKillTargetsProcessesFoundAfterNotice(t *testing.T) {
	b := newBench(t)
	b.procs.running = []procs.Process{{PID: 100, Name: "outlook.exe"}}
	b.onSleep = func() { b.procs.running = []procs.Process{{PID: 200, Name: "outlook.exe"}} }
	b.run(context.Background(), Options{})
	if !slices.Equal(b.procs.killed, []int{200}) {
		t.Fatalf("seul le processus présent après le préavis doit être arrêté, arrêtés : %v", b.procs.killed)
	}
}

// Dépasser max_duration est une erreur (code 2), pas une annulation (code 6).
func TestMaxDurationIsAnError(t *testing.T) {
	b := newBench(t)
	b.cfg.Schedule.MaxDuration.Duration = time.Nanosecond
	time.Sleep(time.Millisecond)
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitError || st.Result != state.ResultError {
		t.Fatalf("code %d (%s) attendu %d", st.ExitCode, st.Result, ExitError)
	}
	if !strings.Contains(st.Message, "durée maximale") {
		t.Errorf("message : %q", st.Message)
	}
}

// Un job sauté n'avance pas la date de dernière réussite globale ; la date par job,
// elle, avance pour les jobs réellement synchronisés.
func TestLastSuccessRequiresEveryJob(t *testing.T) {
	b := newBench(t)
	b.procs.running = []procs.Process{{PID: 7, Name: "outlook.exe"}}
	b.procs.stubborn = true
	st := b.run(context.Background(), Options{})
	if st.LastSuccess != nil {
		t.Error("Outlook non sauvegardé : pas de dernière réussite globale")
	}
	if _, ok := st.JobLastSuccess["data"]; !ok {
		t.Error("le job data a réussi : sa date de réussite doit être enregistrée")
	}
	if _, ok := st.JobLastSuccess["outlook"]; ok {
		t.Error("le job outlook a été sauté : pas de date de réussite")
	}

	b2 := newBench(t)
	st = b2.run(context.Background(), Options{JobIDs: []string{"data"}})
	if st.LastSuccess != nil {
		t.Error("une exécution partielle (--job) n'est pas une sauvegarde complète")
	}
}

// --- bilan ---------------------------------------------------------------------

type fakeReporter struct {
	starts    int
	finishes  []state.Status
	full      []bool
	startFull []bool
	failWith  error
}

func (f *fakeReporter) Name() string { return "faux" }
func (f *fakeReporter) Start(_ context.Context, _ state.Status, full bool) error {
	f.starts++
	f.startFull = append(f.startFull, full)
	return f.failWith
}
func (f *fakeReporter) Finish(_ context.Context, st state.Status, full bool) error {
	f.finishes = append(f.finishes, st)
	f.full = append(f.full, full)
	return f.failWith
}

func (b *bench) runWith(ctx context.Context, opt Options, rep *fakeReporter) state.Status {
	b.t.Helper()
	st, err := Run(ctx, b.cfg, opt, Deps{
		Exec: b.aws, Procs: b.procs, AWSCLI: "aws", Reporter: rep,
		StatusFile:  filepath.Join(b.dir, "status.json"),
		HistoryFile: filepath.Join(b.dir, "history.jsonl"),
		RunsDir:     filepath.Join(b.dir, "logs", "runs"),
		Sleep:       func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return st
}

func TestReporterGetsStartAndFinalStatus(t *testing.T) {
	b := newBench(t)
	rep := &fakeReporter{}
	st := b.runWith(context.Background(), Options{}, rep)
	if rep.starts != 1 || len(rep.finishes) != 1 || !rep.full[0] {
		t.Fatalf("un début et un bilan complet attendus : %+v", rep)
	}
	if f := rep.finishes[0]; f.State != "idle" || f.Result != st.Result || f.LastSuccess == nil {
		t.Errorf("le bilan doit être l'état final : %+v", f)
	}
}

func TestReporterNeverCalledOnDryRun(t *testing.T) {
	b := newBench(t)
	rep := &fakeReporter{}
	b.runWith(context.Background(), Options{DryRun: true}, rep)
	if rep.starts != 0 || len(rep.finishes) != 0 {
		t.Fatalf("une simulation n'envoie aucun bilan : %+v", rep)
	}
}

func TestReporterPartialRunIsFlagged(t *testing.T) {
	b := newBench(t)
	rep := &fakeReporter{}
	b.runWith(context.Background(), Options{JobIDs: []string{"data"}}, rep)
	if len(rep.full) != 1 || rep.full[0] || len(rep.startFull) != 1 || rep.startFull[0] {
		t.Fatalf("--job : début et bilan marqués partiels attendus : %+v / %+v", rep.startFull, rep.full)
	}
}

func TestReporterStillCalledAfterCancel(t *testing.T) {
	b := newBench(t)
	rep := &fakeReporter{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := b.runWith(ctx, Options{}, rep)
	if len(rep.finishes) != 1 || rep.finishes[0].Result != state.ResultCancelled || st.ExitCode != ExitCancelled {
		t.Fatalf("une exécution annulée doit quand même être signalée : %+v", rep.finishes)
	}
}

func TestReporterFailureDoesNotChangeTheResult(t *testing.T) {
	b := newBench(t)
	rep := &fakeReporter{failWith: os.ErrDeadlineExceeded}
	st := b.runWith(context.Background(), Options{}, rep)
	if st.ExitCode != ExitOK {
		t.Fatalf("une supervision injoignable ne doit pas faire échouer la sauvegarde : code %d", st.ExitCode)
	}
	log, _ := os.ReadFile(filepath.Join(b.dir, "logs", "runs", st.RunID, "run.log"))
	if !strings.Contains(string(log), "bilan non envoyé") {
		t.Error("l'échec d'envoi doit être journalisé")
	}
}

func TestAWSEnvReachesEveryCallButNotTheLogs(t *testing.T) {
	b := newBench(t)
	st, err := Run(context.Background(), b.cfg, Options{}, Deps{
		Exec: b.aws, Procs: b.procs, AWSCLI: "aws",
		AWSEnv:      []string{"AWS_ACCESS_KEY_ID=AKIDTEST", "AWS_SECRET_ACCESS_KEY=SECRET-NE-PAS-JOURNALISER"},
		StatusFile:  filepath.Join(b.dir, "status.json"),
		HistoryFile: filepath.Join(b.dir, "history.jsonl"),
		RunsDir:     filepath.Join(b.dir, "logs", "runs"),
		Sleep:       func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	})
	if err != nil || st.ExitCode != ExitOK {
		t.Fatalf("exécution : %v, code %d", err, st.ExitCode)
	}
	if len(b.aws.envs) == 0 {
		t.Fatal("aucun appel à aws")
	}
	for i, env := range b.aws.envs {
		if !slices.Contains(env, "AWS_SECRET_ACCESS_KEY=SECRET-NE-PAS-JOURNALISER") || !slices.Contains(env, "PYTHONUTF8=1") {
			t.Errorf("appel %d : environnement incomplet %v", i, env)
		}
	}
	err = filepath.WalkDir(b.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "SECRET-NE-PAS-JOURNALISER") {
			t.Errorf("clé secrète écrite dans %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// mkLink : jonction sous Windows (aucun privilège requis), lien symbolique ailleurs.
func mkLink(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink : %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// M1 : une jonction posée dans une source (vers le coffre de l'agent, le profil d'un
// autre compte) n'est jamais suivie : elle est passée à aws en exclusion.
func TestLinksInSourceAreExcluded(t *testing.T) {
	b := newBench(t)
	secret := filepath.Join(b.dir, "ProgramData", "Kura", "credentials")
	mk(t, filepath.Join(secret, "wasabi.secret"))
	mkLink(t, secret, filepath.Join(b.outlook, "j [1]"))
	st := b.run(context.Background(), Options{})
	if js := b.job(st, "outlook"); js.Result != state.ResultOK {
		t.Fatalf("%+v", js)
	}
	for _, args := range b.aws.args[call{"C/alice/Outlook", false}] {
		joined := strings.Join(args, "\x00")
		if !strings.Contains(joined, "--exclude\x00j [[]1]\x00--exclude\x00j [[]1]/*") {
			t.Fatalf("jonction non exclue : %q", args)
		}
	}
	if len(b.aws.args[call{"C/alice/Outlook", false}]) == 0 {
		t.Fatal("pas de synchronisation")
	}
	// les autres jobs gardent leurs exclusions telles quelles
	for _, args := range b.aws.args[call{"DATA", false}] {
		if strings.Count(strings.Join(args, " "), "--exclude") != 1 {
			t.Fatalf("exclusions du job data : %q", args)
		}
	}
}

// Un lien posé pendant la synchronisation, suivi par aws : job en erreur, préfixe en
// quarantaine.
func TestLinkAppearingDuringSyncIsQuarantined(t *testing.T) {
	b := newBench(t)
	secret := filepath.Join(b.dir, "ProgramData", "Kura", "credentials")
	mk(t, filepath.Join(secret, "wasabi.secret"))
	link := filepath.Join(b.outlook, "j")
	b.aws.behave["C/alice/Outlook"] = func(dry bool, n int) (string, int) {
		if dry {
			return "", 0
		}
		if _, err := os.Lstat(link); err != nil {
			mkLink(t, secret, link) // posé après le parcours préalable
		}
		return "upload: " + filepath.Join(link, "wasabi.secret") + " to s3://acme-srv01/C/alice/Outlook/j/wasabi.secret\n", 0
	}
	st := b.run(context.Background(), Options{})
	js := b.job(st, "outlook")
	if js.Result != state.ResultError || !strings.Contains(js.Message, "quarantaine") {
		t.Fatalf("%+v", js)
	}
	if len(b.quarantined) != 1 || b.quarantined[0] != "C/alice/Outlook/j" {
		t.Fatalf("quarantaine : %v", b.quarantined)
	}
}
