// Package runner orchestre une exécution de `kura-agent run` :
// pré-tâches, garde-fou, synchronisations, état, journaux et code de sortie.
//
// Il ne dépend d'aucune interface graphique : les dépendances système
// (processus, CLI AWS, horloge) sont injectées, ce qui permet de le tester
// avec une fausse CLI.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/awscli"
	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/guard"
	"github.com/E-C-G/kura-agent/internal/procs"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Codes de sortie de `kura-agent run` (specs §7), visibles dans l'historique du Planificateur.
const (
	ExitOK        = 0 // tous les jobs OK
	ExitWarning   = 1 // fichiers ignorés, job sauté car Outlook ouvert…
	ExitError     = 2 // au moins un job en erreur
	ExitConfig    = 3 // configuration invalide
	ExitBusy      = 4 // une exécution est déjà en cours
	ExitGuard     = 5 // au moins un job bloqué par le garde-fou
	ExitCancelled = 6 // annulée
)

// ExitCodeFor traduit le résultat global en code de sortie.
func ExitCodeFor(result string) int {
	switch result {
	case state.ResultOK:
		return ExitOK
	case state.ResultWarning, state.ResultSkipped:
		return ExitWarning
	case state.ResultGuard:
		return ExitGuard
	case state.ResultCancelled:
		return ExitCancelled
	default:
		return ExitError
	}
}

// Options sont les choix de la ligne de commande.
type Options struct {
	JobIDs       []string // vide = tous les jobs
	DryRun       bool     // simulation : rien n'est copié ni supprimé, Outlook n'est pas fermé
	ForceDeletes bool     // lève le garde-fou de suppressions pour cette exécution
}

// Deps sont les dépendances système, remplaçables en test.
type Deps struct {
	Exec        execx.Executor
	Procs       procs.Manager
	AWSCLI      string // chemin de aws.exe résolu
	StatusFile  string
	HistoryFile string
	RunsDir     string
	Console     io.Writer // copie du journal à l'écran ; nil = aucune

	Now        func() time.Time                                 // défaut : time.Now
	Sleep      func(ctx context.Context, d time.Duration) error // défaut : attente annulable
	Retries    int                                              // nouvelles tentatives ; défaut 2
	RetryDelay time.Duration                                    // défaut 5 min
}

func (d *Deps) defaults() {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = sleepCtx
	}
	if d.Retries == 0 {
		d.Retries = 2
	}
	if d.Retries < 0 {
		d.Retries = 0
	}
	if d.RetryDelay == 0 {
		d.RetryDelay = 5 * time.Minute
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run exécute une sauvegarde complète. Le verrou machine doit être pris par l'appelant.
func Run(ctx context.Context, cfg *config.Config, opt Options, d Deps) (state.Status, error) {
	d.defaults()
	jobs, err := selectJobs(cfg, opt.JobIDs)
	if err != nil {
		return state.Status{}, err
	}

	start := d.Now()
	runID, runDir, err := newRunDir(d.RunsDir, start)
	if err != nil {
		return state.Status{}, fmt.Errorf("dossier de journaux : %w", err)
	}
	runLog, err := os.OpenFile(filepath.Join(runDir, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return state.Status{}, fmt.Errorf("journal : %w", err)
	}
	defer runLog.Close()
	var w io.Writer = runLog
	if d.Console != nil {
		w = io.MultiWriter(runLog, d.Console)
	}
	log := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: levelOf(cfg.Logging.Level)}))

	r := &run{cfg: cfg, opt: opt, d: d, log: log, runDir: runDir}
	r.st = state.Status{State: "running", RunID: runID, DryRun: opt.DryRun, StartedAt: start}
	if prev, err := state.Read(d.StatusFile); err != nil {
		log.Warn("état précédent illisible, il sera remplacé", "err", err)
	} else if prev != nil {
		r.st.LastSuccess = prev.LastSuccess
		r.st.JobLastSuccess = prev.JobLastSuccess
	}
	r.save()

	runCtx := ctx
	if max := cfg.Schedule.MaxDuration.Duration; max > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, max)
		defer cancel()
	}
	if err := execx.KillChildrenOnExit(); err != nil {
		log.Warn("impossible de lier aws.exe à l'agent (Job Object)", "err", err)
	}

	log.Info("début de l'exécution", "run_id", runID, "profil", cfg.Profile.Name,
		"bucket", cfg.Profile.Bucket, "jobs", jobIDs(jobs), "simulation", opt.DryRun, "force_deletes", opt.ForceDeletes)

	preOK := r.preTasks(runCtx, jobs)
	for _, j := range jobs {
		r.st.Jobs = append(r.st.Jobs, r.job(runCtx, j, preOK))
		r.save()
	}
	r.postTasks()

	// Bilan
	results := make([]string, 0, len(r.st.Jobs)+len(r.st.PreTasks))
	for _, j := range r.st.Jobs {
		results = append(results, j.Result)
	}
	for _, p := range r.st.PreTasks {
		results = append(results, p.Result)
	}
	overall := state.Worst(results...)
	if overall == state.ResultSkipped {
		overall = state.ResultWarning // un job sauté n'est pas un échec, mais doit se voir
	}
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		overall = state.Worst(overall, state.ResultError)
		r.st.Message = fmt.Sprintf("durée maximale de %s dépassée", cfg.Schedule.MaxDuration)
	case ctx.Err() != nil:
		overall = state.ResultCancelled
		r.st.Message = "exécution annulée"
	}
	end := d.Now()
	r.st.State = "idle"
	r.st.Result = overall
	r.st.EndedAt = &end
	r.st.ExitCode = ExitCodeFor(overall)
	if !opt.DryRun {
		// Dernière réussite par job : seuls les jobs réellement synchronisés comptent.
		allDone := len(jobs) == len(cfg.Jobs)
		for _, j := range r.st.Jobs {
			if j.Result == state.ResultOK || j.Result == state.ResultWarning {
				if r.st.JobLastSuccess == nil {
					r.st.JobLastSuccess = map[string]time.Time{}
				}
				r.st.JobLastSuccess[j.ID] = end
			} else {
				allDone = false
			}
		}
		// Dernière réussite globale : tous les jobs configurés ont tourné sans erreur.
		// Un job sauté (Outlook resté ouvert) ou une exécution partielle (--job) ne compte pas.
		if allDone {
			r.st.LastSuccess = &end
		}
	}
	r.save()
	if err := state.AppendHistory(d.HistoryFile, r.st); err != nil {
		log.Warn("historique non mis à jour", "err", err)
	}
	purgeRuns(d.RunsDir, cfg.Logging.RetentionDays, end, log)
	log.Info("fin de l'exécution", "résultat", overall, "code", r.st.ExitCode,
		"durée", end.Sub(start).Round(time.Second).String())
	return r.st, nil
}

type run struct {
	cfg    *config.Config
	opt    Options
	d      Deps
	log    *slog.Logger
	runDir string
	st     state.Status
}

func (r *run) save() {
	if err := state.WriteAtomic(r.d.StatusFile, r.st); err != nil {
		r.log.Warn("status.json non écrit", "err", err)
	}
}

// preTasks ferme les processus déclarés et renvoie, par processus, s'il est bien fermé.
func (r *run) preTasks(ctx context.Context, jobs []config.Job) map[string]bool {
	ok := map[string]bool{}
	for _, p := range r.cfg.PreTasks {
		key := strings.ToLower(p.Process)
		if !r.preTaskNeeded(p, jobs) {
			ok[key] = true
			continue
		}
		ps := state.PreTaskStatus{Process: p.Process, Result: state.ResultOK}
		if r.opt.DryRun {
			ps.Message = "simulation : le processus n'est pas fermé"
			r.log.Info("pré-tâche ignorée en simulation", "processus", p.Process)
			r.st.PreTasks = append(r.st.PreTasks, ps)
			ok[key] = true
			continue
		}
		ok[key] = r.closeProcess(ctx, p, &ps)
		r.st.PreTasks = append(r.st.PreTasks, ps)
		r.save()
	}
	return ok
}

// preTaskNeeded : une pré-tâche dont dépendent des jobs ne sert que si l'un d'eux est exécuté ;
// une pré-tâche dont aucun job ne dépend s'exécute toujours.
func (r *run) preTaskNeeded(p config.PreTask, selected []config.Job) bool {
	declared := false
	for _, j := range r.cfg.Jobs {
		if strings.EqualFold(j.DependsOnPreTask, p.Process) {
			declared = true
		}
	}
	if !declared {
		return true
	}
	for _, j := range selected {
		if strings.EqualFold(j.DependsOnPreTask, p.Process) {
			return true
		}
	}
	return false
}

func (r *run) closeProcess(ctx context.Context, p config.PreTask, ps *state.PreTaskStatus) bool {
	found, err := r.d.Procs.Find(p.Process)
	if err != nil {
		ps.Result, ps.Message = state.ResultWarning, "liste des processus impossible : "+err.Error()
		r.log.Warn("pré-tâche : liste des processus impossible", "processus", p.Process, "err", err)
		return false
	}
	ps.Found = len(found)
	if len(found) == 0 {
		r.log.Info("pré-tâche : processus absent, rien à fermer", "processus", p.Process)
		return true
	}
	for _, f := range found {
		ps.Sessions = appendUnique(ps.Sessions, f.Session)
	}
	// V1 sans icône : pas de préavis affiché ni de fermeture douce possibles depuis
	// la session de svc-kura. On attend le préavis puis on force (comme taskkill /F).
	r.log.Info("pré-tâche : processus ouvert, attente du préavis avant arrêt forcé",
		"processus", p.Process, "instances", len(found), "sessions", ps.Sessions,
		"préavis_s", p.NoticeSeconds, "note", "fermeture douce et report disponibles avec l'icône")
	if err := r.d.Sleep(ctx, time.Duration(p.NoticeSeconds)*time.Second); err != nil {
		ps.Result, ps.Message = r.interrupted(ctx), "interrompu pendant le préavis : "+r.interruptReason(ctx)
		return false
	}
	if !p.Force {
		ps.Result, ps.Message = state.ResultWarning, "processus toujours ouvert (force: false)"
		r.log.Warn("pré-tâche : processus laissé ouvert (force: false)", "processus", p.Process)
		return false
	}
	// Rechercher de nouveau : pendant le préavis, l'utilisateur a pu fermer Outlook et
	// Windows réattribuer son PID à un autre processus. On n'arrête que ce qui tourne
	// encore sous ce nom (Terminate revérifie le nom sur le processus lui-même).
	found, err = r.d.Procs.Find(p.Process)
	if err != nil {
		ps.Result, ps.Message = state.ResultWarning, "liste des processus impossible : "+err.Error()
		return false
	}
	if len(found) == 0 {
		r.log.Info("pré-tâche : processus fermé pendant le préavis", "processus", p.Process)
		return true
	}
	for _, f := range found {
		if err := r.d.Procs.Terminate(f); err != nil {
			r.log.Warn("pré-tâche : arrêt impossible", "pid", f.PID, "session", f.Session, "err", err)
			continue
		}
		ps.Closed++
		r.log.Info("pré-tâche : processus arrêté", "processus", p.Process, "pid", f.PID, "session", f.Session)
	}
	still, err := r.d.Procs.Find(p.Process)
	if err == nil && len(still) == 0 {
		return true
	}
	ps.Result = state.ResultWarning
	ps.Message = fmt.Sprintf("%d instance(s) toujours ouverte(s)", len(still))
	r.log.Warn("pré-tâche : processus toujours ouvert après arrêt forcé", "processus", p.Process, "instances", len(still))
	return false
}

func (r *run) job(ctx context.Context, j config.Job, preOK map[string]bool) state.JobStatus {
	js := state.JobStatus{ID: j.ID, Name: j.Name, Result: state.ResultOK}
	start := r.d.Now()
	defer func() { js.DurationS = r.d.Now().Sub(start).Seconds() }()
	log := r.log.With("job", j.ID)

	if ctx.Err() != nil {
		js.Result, js.Message = r.interrupted(ctx), "non exécuté : "+r.interruptReason(ctx)
		return js
	}
	if dep := j.DependsOnPreTask; dep != "" && !preOK[strings.ToLower(dep)] {
		js.Result, js.Message = state.ResultSkipped, dep+" n'a pas pu être fermé : job sauté"
		log.Warn("job sauté", "raison", js.Message)
		return js
	}

	jobLog, err := os.OpenFile(filepath.Join(r.runDir, j.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		js.Result, js.Message = state.ResultError, "journal du job impossible : "+err.Error()
		return js
	}
	defer jobLog.Close()

	if r.opt.DryRun {
		counts, ok := r.preflight(ctx, j, jobLog, log, &js)
		if !ok {
			return js
		}
		if counts == nil { // job sans garde-fou : simulation dédiée
			c, ok := r.simulate(ctx, j, jobLog, log, &js)
			if !ok {
				return js
			}
			counts = &c
		}
		js.Uploaded, js.Deleted, js.Skipped = counts.Uploaded, counts.Deleted, counts.Skipped
		js.Message = fmt.Sprintf("simulation : %d envoi(s) et %d suppression(s) prévus", counts.Uploaded, counts.Deleted)
		log.Info("simulation terminée", "envois", counts.Uploaded, "suppressions", counts.Deleted)
		return js
	}

	// Synchronisation réelle. Le contrôle préalable (source + garde-fou) est refait
	// avant CHAQUE tentative : entre deux essais espacés de plusieurs minutes, le disque
	// a pu être démonté ou vidé, et --delete le propagerait au bucket.
	for attempt := 0; ; attempt++ {
		if _, ok := r.preflight(ctx, j, jobLog, log, &js); !ok {
			return js
		}
		fmt.Fprintf(jobLog, "== synchronisation %s (tentative %d) ==\n", r.d.Now().Format(time.RFC3339), attempt+1)
		counts, code, err := r.syncOnce(ctx, j, false, jobLog)
		js.Attempts++
		js.ExitCode = code
		js.Uploaded, js.Deleted, js.Skipped = counts.Uploaded, counts.Deleted, counts.Skipped
		if err != nil {
			js.Result, js.Message = r.failureOf(ctx, err), err.Error()
			break
		}
		outcome := awscli.Classify(code)
		if outcome == awscli.OK {
			js.Result, js.Message = state.ResultOK, ""
			break
		}
		if outcome == awscli.Warning {
			js.Result = state.ResultWarning
			js.Message = fmt.Sprintf("%d fichier(s) ignoré(s), souvent verrouillés ; voir %s.log", counts.Skipped, j.ID)
			break
		}
		js.Result = state.ResultError
		js.Message = fmt.Sprintf("aws s3 sync a échoué (code %d) ; voir %s.log", code, j.ID)
		if outcome == awscli.Failed || attempt >= r.d.Retries {
			break
		}
		log.Warn("nouvelle tentative", "dans", r.d.RetryDelay.String(), "tentative", attempt+2, "code_précédent", code)
		if err := r.d.Sleep(ctx, r.d.RetryDelay); err != nil {
			js.Result, js.Message = r.interrupted(ctx), "interrompu avant une nouvelle tentative : "+r.interruptReason(ctx)
			break
		}
	}
	log.Info("job terminé", "résultat", js.Result, "envois", js.Uploaded, "suppressions", js.Deleted,
		"ignorés", js.Skipped, "code", js.ExitCode, "tentatives", js.Attempts)
	return js
}

// preflight vérifie la source puis, pour un job avec delete, applique le garde-fou :
// comptage, simulation, seuils. Il renvoie les comptes de la simulation (nil si le job
// n'a pas de garde-fou) et false si le job doit s'arrêter, js étant alors renseigné.
func (r *run) preflight(ctx context.Context, j config.Job, out io.Writer, log *slog.Logger, js *state.JobStatus) (*awscli.Counts, bool) {
	if st, err := os.Stat(j.Source); err != nil || !st.IsDir() {
		js.Result, js.Message = state.ResultError, fmt.Sprintf("source %q inaccessible", j.Source)
		log.Error("source inaccessible", "source", j.Source, "err", err)
		return nil, false
	}
	if !j.Delete || j.Guard == nil {
		return nil, true
	}
	census, err := guard.CountFiles(j.Source, j.Exclude)
	if err != nil {
		js.Result, js.Message = state.ResultError, err.Error()
		log.Error("garde-fou : comptage impossible", "err", err)
		return nil, false
	}
	if census.Unreadable > 0 {
		log.Info("garde-fou : éléments illisibles ignorés au comptage", "nombre", census.Unreadable)
	}
	counts, ok := r.simulate(ctx, j, out, log, js)
	if !ok {
		js.Message = "garde-fou non évaluable : " + js.Message
		return nil, false
	}
	dec := guard.Evaluate(*j.Guard, census.Files, counts.Deleted)
	log.Info("garde-fou", "fichiers_source", census.Files, "suppressions_prévues", counts.Deleted, "bloqué", dec.Blocked)
	if !dec.Blocked {
		return &counts, true
	}
	// --force-deletes lève les plafonds de suppressions, jamais le minimum de fichiers :
	// une source vide ou démontée n'est pas un cas à forcer.
	if r.opt.ForceDeletes && dec.Kind != guard.TooFewFiles {
		log.Warn("garde-fou levé par --force-deletes", "raison", dec.Reason)
		return &counts, true
	}
	js.Result, js.Deleted = state.ResultGuard, counts.Deleted
	js.Message = dec.Reason
	if dec.Kind != guard.TooFewFiles {
		js.Message += " ; après vérification : kura-agent run --job " + j.ID + " --force-deletes"
	}
	log.Error("garde-fou : job bloqué, rien n'est modifié dans le bucket", "raison", dec.Reason)
	return nil, false
}

// simulate lance aws s3 sync --dryrun, avec nouvelles tentatives sur les erreurs passagères.
func (r *run) simulate(ctx context.Context, j config.Job, out io.Writer, log *slog.Logger, js *state.JobStatus) (awscli.Counts, bool) {
	for attempt := 0; ; attempt++ {
		fmt.Fprintf(out, "== simulation %s ==\n", r.d.Now().Format(time.RFC3339))
		counts, code, err := r.syncOnce(ctx, j, true, out)
		js.Attempts++
		js.ExitCode = code
		if err != nil {
			js.Result, js.Message = r.failureOf(ctx, err), err.Error()
			return counts, false
		}
		outcome := awscli.Classify(code)
		if outcome == awscli.OK || outcome == awscli.Warning {
			return counts, true
		}
		js.Result, js.Message = state.ResultError, fmt.Sprintf("simulation en échec (code %d)", code)
		if outcome == awscli.Failed || attempt >= r.d.Retries {
			log.Error("simulation en échec", "code", code)
			return counts, false
		}
		log.Warn("simulation : nouvelle tentative", "dans", r.d.RetryDelay.String(), "code_précédent", code)
		if err := r.d.Sleep(ctx, r.d.RetryDelay); err != nil {
			js.Result, js.Message = r.interrupted(ctx), r.interruptReason(ctx)
			return counts, false
		}
	}
}

// syncOnce lance une fois aws s3 sync et journalise commande, sortie et code.
func (r *run) syncOnce(ctx context.Context, j config.Job, dryRun bool, out io.Writer) (awscli.Counts, int, error) {
	args := awscli.SyncArgs(r.cfg.Profile, j, dryRun)
	fmt.Fprintf(out, "$ %s %s\n", r.d.AWSCLI, strings.Join(quoteArgs(args), " "))
	c := awscli.NewCounter(out)
	code, err := r.d.Exec.Run(ctx, r.d.AWSCLI, args, awscli.Env(r.cfg.Profile), c)
	fmt.Fprintf(out, "== code de sortie %d ==\n", code)
	if err != nil {
		return c.Counts(), code, fmt.Errorf("lancement de aws : %w", err)
	}
	return c.Counts(), code, nil
}

// interrupted : un dépassement de max_duration est une erreur ; une annulation, cancelled.
func (r *run) interrupted(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return state.ResultError
	}
	return state.ResultCancelled
}

func (r *run) interruptReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("durée maximale de %s dépassée", r.cfg.Schedule.MaxDuration)
	}
	return "exécution annulée"
}

// failureOf classe une erreur de lancement : interruption ou erreur.
func (r *run) failureOf(ctx context.Context, err error) string {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return r.interrupted(ctx)
	}
	return state.ResultError
}

// postTasks : relancer Outlook dans la session d'un utilisateur demande l'icône (étape 5).
func (r *run) postTasks() {
	if r.opt.DryRun {
		return
	}
	for _, p := range r.cfg.PostTasks {
		wasRunning := false
		for _, ps := range r.st.PreTasks {
			if strings.EqualFold(ps.Process, p.Process) && ps.Closed > 0 {
				wasRunning = true
			}
		}
		if p.OnlyIfWasRunning && !wasRunning {
			continue
		}
		r.log.Info("post-tâche non exécutée : la relance dans la session de l'utilisateur passe par l'icône",
			"processus", p.Process)
	}
}

func selectJobs(cfg *config.Config, ids []string) ([]config.Job, error) {
	if len(ids) == 0 {
		return cfg.Jobs, nil
	}
	var out []config.Job
	for _, id := range ids {
		j, ok := cfg.Job(id)
		if !ok {
			return nil, fmt.Errorf("job %q inconnu (jobs disponibles : %s)", id, strings.Join(jobIDs(cfg.Jobs), ", "))
		}
		out = append(out, j)
	}
	return out, nil
}

func jobIDs(jobs []config.Job) []string {
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids
}

func newRunDir(base string, t time.Time) (string, string, error) {
	id := t.Format("2006-01-02_1504")
	for n := 1; ; n++ {
		runID := id
		if n > 1 {
			runID = fmt.Sprintf("%s-%d", id, n)
		}
		dir := filepath.Join(base, runID)
		if err := os.MkdirAll(base, 0o755); err != nil {
			return "", "", err
		}
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return runID, dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", "", err
		}
	}
}

// purgeRuns supprime les dossiers de journaux plus vieux que la durée de conservation.
func purgeRuns(base string, days int, now time.Time, log *slog.Logger) {
	if days <= 0 {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	limit := now.AddDate(0, 0, -days)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(limit) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(base, e.Name())); err != nil {
			log.Warn("purge des journaux impossible", "dossier", e.Name(), "err", err)
		}
	}
}

func levelOf(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\"*$") {
			out[i] = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		} else {
			out[i] = a
		}
	}
	return out
}

func appendUnique(s []int, v int) []int {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
