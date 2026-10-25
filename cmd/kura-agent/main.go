// Commande kura-agent : sauvegarde planifiée vers Wasabi (wrapper de `aws s3 sync`).
//
// Étapes livrées : run, validate, status, version.
// À venir : webhook, install / uninstall / credentials, tray, restore.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/procs"
	"github.com/E-C-G/kura-agent/internal/runner"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Renseignés à la compilation : -ldflags "-X main.version=… -X main.commit=… -X main.date=…"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `kura-agent — sauvegarde planifiée vers Wasabi

Usage :
  kura-agent run      [--config FICHIER] [--job ID]... [--dry-run] [--force-deletes] [--quiet]
  kura-agent validate [--config FICHIER] [--offline]
  kura-agent status   [--json]
  kura-agent version

Commandes :
  run       exécute la sauvegarde (normalement lancée par la tâche planifiée \Kura\Sync)
  validate  vérifie la configuration, la présence d'aws.exe et l'accès au bucket
  status    affiche l'état de la dernière exécution
  version   affiche la version

Codes de sortie de run : 0 OK · 1 avertissement · 2 erreur · 3 configuration invalide
                         4 déjà en cours · 5 garde-fou · 6 annulée
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "run":
		code = cmdRun(os.Args[2:])
	case "validate":
		code = cmdValidate(os.Args[2:])
	case "status":
		code = cmdStatus(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("kura-agent %s (commit %s, compilé le %s)\n", version, commit, date)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "commande inconnue %q\n\n%s", os.Args[1], usage)
		code = 2
	}
	os.Exit(code)
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*m = append(*m, p)
		}
	}
	return nil
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", paths.ConfigFile(), "fichier de configuration")
	var jobs multiFlag
	fs.Var(&jobs, "job", "n'exécuter que ce job (répétable, ou liste séparée par des virgules)")
	dryRun := fs.Bool("dry-run", false, "simulation : rien n'est copié ni supprimé, Outlook n'est pas fermé")
	force := fs.Bool("force-deletes", false, "lever le garde-fou de suppressions pour cette exécution")
	quiet := fs.Bool("quiet", false, "ne pas recopier le journal à l'écran")
	if err := fs.Parse(args); err != nil {
		return runner.ExitConfig
	}

	cfg, err := config.Load(*cfgPath, config.LoadOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		recordConfigError(err)
		return runner.ExitConfig
	}
	aws, err := resolveAWS(cfg.Profile.AWSCLI)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		recordConfigError(err)
		return runner.ExitConfig
	}

	release, err := lock.Acquire()
	if errors.Is(err, lock.ErrBusy) {
		fmt.Fprintln(os.Stderr, "une exécution est déjà en cours : abandon")
		return runner.ExitBusy
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "verrou impossible :", err)
		return runner.ExitError
	}
	defer release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var console io.Writer = os.Stdout
	if *quiet {
		console = nil
	}
	st, err := runner.Run(ctx, cfg, runner.Options{JobIDs: jobs, DryRun: *dryRun, ForceDeletes: *force}, runner.Deps{
		Exec:        execx.System{},
		Procs:       procs.System(),
		AWSCLI:      aws,
		StatusFile:  paths.StatusFile(),
		HistoryFile: paths.HistoryFile(),
		RunsDir:     paths.RunsDir(),
		Console:     console,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return runner.ExitConfig
	}
	printSummary(os.Stdout, &st)
	return st.ExitCode
}

// recordConfigError publie l'erreur dans status.json pour que l'icône l'affiche.
func recordConfigError(cause error) {
	now := time.Now()
	st := state.Status{State: "idle", RunID: now.Format("2006-01-02_1504"), Result: state.ResultError,
		StartedAt: now, EndedAt: &now, ExitCode: runner.ExitConfig, Message: cause.Error(), Jobs: []state.JobStatus{}}
	if prev, _ := state.Read(paths.StatusFile()); prev != nil {
		st.LastSuccess = prev.LastSuccess
	}
	_ = state.WriteAtomic(paths.StatusFile(), st)
}

func resolveAWS(configured string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("profile.aws_cli : %q introuvable (AWS CLI v2 installée ?)", configured)
		}
		return configured, nil
	}
	p, err := exec.LookPath("aws")
	if err != nil {
		return "", errors.New("aws introuvable dans le PATH : installer AWS CLI v2 ou renseigner profile.aws_cli")
	}
	return p, nil
}

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	cfgPath := fs.String("config", paths.ConfigFile(), "fichier de configuration")
	offline := fs.Bool("offline", false, "ne pas tester l'accès au bucket")
	if err := fs.Parse(args); err != nil {
		return runner.ExitConfig
	}
	ok := true
	check := func(label string, err error) {
		if err != nil {
			ok = false
			fmt.Printf("ÉCHEC  %s\n       %s\n", label, strings.ReplaceAll(err.Error(), "\n", "\n       "))
			return
		}
		fmt.Printf("OK     %s\n", label)
	}

	cfg, err := config.Load(*cfgPath, config.LoadOptions{CheckSystem: true})
	check("configuration "+*cfgPath, err)
	if cfg == nil {
		return runner.ExitConfig
	}
	aws, err := resolveAWS(cfg.Profile.AWSCLI)
	check("AWS CLI", err)
	if err == nil {
		out, err := exec.Command(aws, "--version").CombinedOutput()
		if err == nil {
			fmt.Printf("       %s\n", strings.TrimSpace(string(out)))
		}
		check("AWS CLI exécutable", err)
		if !*offline {
			cmd := exec.Command(aws, "s3api", "head-bucket", "--bucket", cfg.Profile.Bucket,
				"--endpoint-url", cfg.Profile.Endpoint)
			cmd.Env = append(os.Environ(), "AWS_PAGER=")
			if cfg.Profile.AWSProfile != "" {
				cmd.Env = append(cmd.Env, "AWS_PROFILE="+cfg.Profile.AWSProfile)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				err = fmt.Errorf("%v : %s", err, strings.TrimSpace(string(out)))
			}
			check(fmt.Sprintf("accès au bucket %s (profil %q)", cfg.Profile.Bucket, cfg.Profile.AWSProfile), err)
		}
	}
	if !ok {
		return runner.ExitConfig
	}
	return runner.ExitOK
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "sortie JSON brute")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := state.Read(paths.StatusFile())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if st == nil {
		fmt.Println("aucune exécution enregistrée")
		return 0
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st)
		return 0
	}
	printSummary(os.Stdout, st)
	return 0
}

var resultLabel = map[string]string{
	state.ResultOK: "OK", state.ResultWarning: "avertissement", state.ResultError: "erreur",
	state.ResultGuard: "bloqué par le garde-fou", state.ResultCancelled: "annulé", state.ResultSkipped: "sauté",
}

func label(r string) string {
	if l, ok := resultLabel[r]; ok {
		return l
	}
	return r
}

func printSummary(w io.Writer, st *state.Status) {
	fmt.Fprintf(w, "\nExécution %s : %s (code %d)", st.RunID, label(st.Result), st.ExitCode)
	if st.DryRun {
		fmt.Fprint(w, " — simulation")
	}
	fmt.Fprintln(w)
	if st.State == "running" {
		fmt.Fprintln(w, "  en cours")
	}
	if st.Message != "" {
		fmt.Fprintf(w, "  %s\n", st.Message)
	}
	for _, p := range st.PreTasks {
		fmt.Fprintf(w, "  pré-tâche %-12s %s : %d trouvé(s), %d arrêté(s) %s\n", p.Process, label(p.Result), p.Found, p.Closed, p.Message)
	}
	for _, j := range st.Jobs {
		fmt.Fprintf(w, "  job %-10s %-24s envois %d · suppressions %d · %.0f s", j.ID, label(j.Result), j.Uploaded, j.Deleted, j.DurationS)
		if j.Message != "" {
			fmt.Fprintf(w, "\n             %s", j.Message)
		}
		fmt.Fprintln(w)
	}
	if st.LastSuccess != nil {
		fmt.Fprintf(w, "  dernière sauvegarde réussie : %s\n", st.LastSuccess.Local().Format("02/01/2006 15:04"))
	}
}
