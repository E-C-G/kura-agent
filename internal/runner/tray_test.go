package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/ipc"
	"github.com/E-C-G/kura-agent/internal/procs"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// fakeTrays joue les icônes : une par session listée dans present.
type fakeTrays struct {
	mu        sync.Mutex
	present   map[int]bool
	procs     *fakeProcs
	choice    func(m ipc.Msg) string // réponse au préavis ; défaut : fermer maintenant
	silent    bool                   // ne répond jamais (délai écoulé)
	stubborn  bool                   // la fermeture douce n'aboutit pas
	onClose   func()                 // appelé après la fermeture douce
	events    []ipc.Msg
	requests  []ipc.Msg
	sessionOf []int
}

func (f *fakeTrays) Broadcast(m ipc.Msg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, m)
}

func (f *fakeTrays) Has(s int) bool { return f.present[s] }

func (f *fakeTrays) Request(ctx context.Context, s int, m ipc.Msg) (ipc.Msg, error) {
	f.mu.Lock()
	f.requests = append(f.requests, m)
	f.sessionOf = append(f.sessionOf, s)
	f.mu.Unlock()
	if !f.present[s] {
		return ipc.Msg{}, ipc.ErrNoTray
	}
	if f.silent {
		return ipc.Msg{}, context.DeadlineExceeded
	}
	switch m.Type {
	case ipc.TypePromptClose:
		c := ipc.ChoiceCloseNow
		if f.choice != nil {
			c = f.choice(m)
		}
		return ipc.Msg{Type: ipc.TypeAnswer, Choice: c}, nil
	case ipc.TypeClose:
		if !f.stubborn {
			f.procs.mu.Lock()
			f.procs.running = slices.DeleteFunc(f.procs.running, func(p procs.Process) bool {
				return p.Session == s && strings.EqualFold(p.Name, m.Process)
			})
			f.procs.mu.Unlock()
		}
		if f.onClose != nil {
			f.onClose()
			return ipc.Msg{}, ctx.Err()
		}
		return ipc.Msg{Type: ipc.TypeClosed}, nil
	case ipc.TypeStart:
		return ipc.Msg{Type: ipc.TypeStartedProc, OK: true}, nil
	}
	return ipc.Msg{}, errors.New("requête inattendue " + m.Type)
}

func (f *fakeTrays) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		out = append(out, r.Type)
	}
	return out
}

func trayBench(t *testing.T) (*bench, *fakeTrays) {
	b := newBench(t)
	b.cfg.PostTasks = []config.PostTask{{Type: "start_process", Process: "outlook.exe", OnlyIfWasRunning: true}}
	b.procs.running = []procs.Process{{PID: 42, Name: "outlook.exe", Session: 2,
		Path: `C:\Program Files\Microsoft Office\root\Office16\OUTLOOK.EXE`}}
	ft := &fakeTrays{present: map[int]bool{2: true}, procs: b.procs}
	b.trays = ft
	return b, ft
}

func TestTrayClosesOutlookGracefullyAndRestartsIt(t *testing.T) {
	b, ft := trayBench(t)
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitOK {
		t.Fatalf("code %d : %+v", st.ExitCode, st)
	}
	if len(b.procs.killed) != 0 {
		t.Fatalf("arrêt forcé malgré la fermeture douce : %v", b.procs.killed)
	}
	if len(b.sleeps) != 0 {
		t.Fatalf("attente inutile : l'icône a répondu (%v)", b.sleeps)
	}
	ps := st.PreTasks[0]
	if ps.Closed != 1 || ps.Graceful != 1 {
		t.Fatalf("pré-tâche %+v", ps)
	}
	if got := strings.Join(ft.types(), " "); got != "prompt_close close start" {
		t.Fatalf("requêtes à l'icône : %s", got)
	}
	if ft.requests[0].NoticeS != 120 || ft.requests[1].GracefulS != 60 {
		t.Fatalf("préavis ou délai de fermeture : %+v", ft.requests[:2])
	}
	if !strings.HasSuffix(ft.requests[2].Path, `OUTLOOK.EXE`) {
		t.Fatalf("relance sans chemin : %+v", ft.requests[2])
	}
	var ev []string
	for _, e := range ft.events {
		ev = append(ev, e.Type)
	}
	if strings.Join(ev, " ") != "started job job finished" || ft.events[3].Result != state.ResultOK {
		t.Fatalf("événements : %v / %+v", ev, ft.events)
	}
	if ft.events[2].Index != 2 || ft.events[2].Total != 2 {
		t.Fatalf("progression : %+v", ft.events[2])
	}
	if st.Current != nil {
		t.Fatal("job en cours encore renseigné à la fin")
	}
}

func TestPostponeIsLimited(t *testing.T) {
	b, ft := trayBench(t)
	b.cfg.PreTasks[0].AllowPostpone = &config.Postpone{Delay: config.Duration{Duration: time.Hour}, Max: 2}
	ft.choice = func(ipc.Msg) string { return ipc.ChoicePostpone } // l'utilisateur reporte toujours
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitOK || st.PreTasks[0].Postponed != 2 {
		t.Fatalf("code %d, pré-tâche %+v", st.ExitCode, st.PreTasks[0])
	}
	if !slices.Equal(b.sleeps, []time.Duration{time.Hour, time.Hour}) {
		t.Fatalf("attentes %v", b.sleeps)
	}
	var left []int
	for _, r := range ft.requests {
		if r.Type == ipc.TypePromptClose {
			left = append(left, r.PostponesLeft)
		}
	}
	if !slices.Equal(left, []int{2, 1, 0}) {
		t.Fatalf("reports restants annoncés : %v", left)
	}
}

func TestSilentTrayFallsBackToForce(t *testing.T) {
	b, ft := trayBench(t)
	ft.silent = true
	st := b.run(context.Background(), Options{})
	if len(b.procs.killed) != 1 || st.PreTasks[0].Closed != 1 || st.PreTasks[0].Graceful != 0 {
		t.Fatalf("arrêt forcé attendu : tués %v, %+v", b.procs.killed, st.PreTasks[0])
	}
	if len(b.sleeps) != 0 {
		t.Fatalf("le préavis s'est déjà écoulé pendant l'attente de réponse : %v", b.sleeps)
	}
}

func TestStubbornOutlookIsForced(t *testing.T) {
	b, ft := trayBench(t)
	ft.stubborn = true
	st := b.run(context.Background(), Options{})
	if len(b.procs.killed) != 1 || st.ExitCode != ExitOK {
		t.Fatalf("arrêt forcé après une fermeture douce sans effet : %v, %+v", b.procs.killed, st)
	}
	// Arrêté de force : relancé quand même.
	if got := strings.Join(ft.types(), " "); got != "prompt_close close start" {
		t.Fatalf("requêtes : %s", got)
	}
}

func TestNoTrayInSessionKeepsOldBehaviour(t *testing.T) {
	b, ft := trayBench(t)
	ft.present = map[int]bool{}
	st := b.run(context.Background(), Options{})
	if !slices.Equal(b.sleeps, []time.Duration{120 * time.Second}) || len(b.procs.killed) != 1 {
		t.Fatalf("préavis puis arrêt forcé attendus : %v, tués %v", b.sleeps, b.procs.killed)
	}
	if st.ExitCode != ExitOK {
		t.Fatalf("code %d", st.ExitCode)
	}
	if slices.Contains(ft.types(), ipc.TypeStart) {
		t.Fatal("relance demandée sans icône")
	}
}

func TestTwoSessionsAreHandledTogether(t *testing.T) {
	b, ft := trayBench(t)
	b.procs.running = append(b.procs.running, procs.Process{PID: 43, Name: "outlook.exe", Session: 3})
	ft.present = map[int]bool{2: true} // session 3 sans icône
	st := b.run(context.Background(), Options{})
	ps := st.PreTasks[0]
	if ps.Found != 2 || ps.Closed != 2 || ps.Graceful != 1 || !slices.Equal(b.procs.killed, []int{43}) {
		t.Fatalf("pré-tâche %+v, tués %v", ps, b.procs.killed)
	}
}

func TestCancelCauseIsReported(t *testing.T) {
	b, _ := trayBench(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("annulée depuis l'icône par alice"))
	st := b.run(ctx, Options{})
	if st.Result != state.ResultCancelled || st.Message != "annulée depuis l'icône par alice" {
		t.Fatalf("résultat %s, message %q", st.Result, st.Message)
	}
}

func TestDryRunNeverPromptsButReportsProgress(t *testing.T) {
	b, ft := trayBench(t)
	b.run(context.Background(), Options{DryRun: true})
	if len(ft.requests) != 0 {
		t.Fatalf("requêtes en simulation : %v", ft.types())
	}
	if len(ft.events) == 0 || !ft.events[0].DryRun {
		t.Fatalf("événements : %+v", ft.events)
	}
}

func TestOutlookRestartedAfterCancelDuringClose(t *testing.T) {
	b, ft := trayBench(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	ft.onClose = func() { cancel(errors.New("annulée depuis l'icône par alice")) }
	st := b.run(ctx, Options{})
	if st.Result != state.ResultCancelled {
		t.Fatalf("résultat %s", st.Result)
	}
	if !slices.Contains(ft.types(), ipc.TypeStart) {
		t.Fatalf("Outlook fermé par l'icône puis annulation : relance attendue, requêtes %v", ft.types())
	}
}

func TestUserClosedOutlookDuringCountdownIsNotRestarted(t *testing.T) {
	b, ft := trayBench(t)
	ft.choice = func(ipc.Msg) string {
		b.procs.mu.Lock()
		b.procs.running = nil // l'utilisateur ferme Outlook lui-même
		b.procs.mu.Unlock()
		return ipc.ChoiceTimeout
	}
	st := b.run(context.Background(), Options{})
	if st.ExitCode != ExitOK || st.PreTasks[0].Closed != 0 || slices.Contains(ft.types(), ipc.TypeStart) {
		t.Fatalf("aucune relance attendue : %+v, requêtes %v", st.PreTasks[0], ft.types())
	}
}

func TestAttemptsCountOnlyRealSyncs(t *testing.T) {
	b := newBench(t)
	st := b.run(context.Background(), Options{})
	for _, j := range st.Jobs {
		if j.Attempts != 1 {
			t.Errorf("job %s : %d tentative(s) pour une nuit normale (la simulation du garde-fou ne compte pas)", j.ID, j.Attempts)
		}
	}
}

func TestUnavailableJobFailsAlone(t *testing.T) {
	b := newBench(t)
	b.cfg.Jobs[1].Unavailable = "jobs[1].source : ${PROFILE:bob} : profil introuvable"
	st := b.run(context.Background(), Options{})
	if b.job(st, "data").Result != state.ResultOK || b.job(st, "outlook").Result != state.ResultError {
		t.Fatalf("jobs : %+v", st.Jobs)
	}
	if st.ExitCode != ExitError || b.aws.real("C/alice/Outlook") != 0 {
		t.Fatalf("code %d, appels outlook %d", st.ExitCode, b.aws.real("C/alice/Outlook"))
	}
}
