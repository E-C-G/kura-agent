// Commande kura-agent : sauvegarde planifiée vers Wasabi (wrapper de `aws s3 sync`).
//
// Étapes livrées : run, validate, status, credentials, install, uninstall, version ;
// bilan vers healthchecks ou webhook. À venir : tray, restore.
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
	"path/filepath"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/awscli"
	"github.com/E-C-G/kura-agent/internal/awsinstall"
	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/ipc"
	"github.com/E-C-G/kura-agent/internal/launchd"
	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/platform"
	"github.com/E-C-G/kura-agent/internal/procs"
	"github.com/E-C-G/kura-agent/internal/report"
	"github.com/E-C-G/kura-agent/internal/runner"
	"github.com/E-C-G/kura-agent/internal/setup"
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
  kura-agent run      [--config FICHIER] [--job ID]... [--dry-run] [--force-deletes --job ID] [--quiet] [--scheduled [--catch-up]]
  kura-agent validate [--config FICHIER] [--offline]
  kura-agent status   [--json]
  kura-agent credentials set NOM [--from-env VARIABLE]
  kura-agent credentials set-aws NOM
  kura-agent credentials delete NOM
  kura-agent credentials list
  kura-agent import   FICHIER.kura [--config FICHIER] [--no-test] [--force] [--keep]
  kura-agent install   [--config FICHIER] [--skip-validate]
  kura-agent uninstall [--purge]
  kura-agent suspend  DURÉE | tomorrow | off
  kura-agent tray     [--config FICHIER]
  kura-agent self-update --check [--version vX.Y.Z] [--json] [--config FICHIER]
  kura-agent version

Commandes :
  run          exécute la sauvegarde (normalement lancée par la tâche planifiée \Kura\Sync)
  validate     vérifie la configuration, la présence d'aws.exe et l'accès au bucket
  status       affiche l'état de la dernière exécution
  credentials  gère les secrets référencés par credential:NOM dans la configuration
               (set-aws : paire de clés d'accès Wasabi) ; console d'administration
  import       range dans le coffre la clé d'un paquet .kura préparé par la console kura-ctl
               (mot de passe affiché par la console) ; la clé est testée avant de
               remplacer l'ancienne ; console d'administration
  install      installe ou met à jour : exécutable, configuration,
               secrets manquants, validate, tâche \Kura\Sync ; console d'administration
  uninstall    retire la tâche, le compte et l'exécutable ; --purge efface aussi les données
  suspend      suspend la planification (ex. 1h, tomorrow = demain 08:00) ; off pour reprendre
  tray         icône de la barre de notification (kura-agentw.exe, lancée à l'ouverture de session)
  self-update  --check : cherche une version plus récente sur GitHub et vérifie son
               attestation de release, sans rien modifier (code 3 : mise à jour disponible)
  version      affiche la version

Codes de sortie de run : 0 OK · 1 avertissement · 2 erreur · 3 configuration invalide
                         4 déjà en cours · 5 garde-fou · 6 annulée
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if _, ok := commandNeeds[os.Args[1]]; ok && !checkPlatform(os.Args[1], os.Stderr) {
		os.Exit(unsupportedCode(os.Args[1]))
	}
	var code int
	switch os.Args[1] {
	case "run":
		code = cmdRun(os.Args[2:])
	case "validate":
		code = cmdValidate(os.Args[2:])
	case "status":
		code = cmdStatus(os.Args[2:])
	case "credentials":
		code = cmdCredentials(os.Args[2:])
	case "install":
		code = cmdInstall(os.Args[2:])
	case "import":
		code = cmdImport(os.Args[2:])
	case "suspend":
		code = cmdSuspend(os.Args[2:])
	case "tray":
		code = cmdTray(os.Args[2:])
	case "uninstall":
		code = cmdUninstall(os.Args[2:])
	case "self-update":
		code = cmdSelfUpdate(os.Args[2:])
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
	force := fs.Bool("force-deletes", false, "lever les plafonds de suppressions du garde-fou (avec --job)")
	quiet := fs.Bool("quiet", false, "ne pas recopier le journal à l'écran")
	scheduled := fs.Bool("scheduled", false, "lancement par la tâche planifiée : respecte la suspension (kura-agent suspend)")
	catchUp := fs.Bool("catch-up", false, "avec --scheduled, lancement au démarrage (macOS) : ne sauvegarde que si un passage prévu a été manqué")
	if err := fs.Parse(args); err != nil {
		return runner.ExitConfig
	}
	if *catchUp && !*scheduled {
		fmt.Fprintln(os.Stderr, "--catch-up s'emploie avec --scheduled")
		return runner.ExitConfig
	}
	if *scheduled {
		if sp, err := state.ReadSuspension(paths.SuspendFile()); err == nil && sp.Active(time.Now()) {
			fmt.Printf("planification suspendue jusqu'au %s : rien à faire\n", sp.Until.Local().Format("02/01/2006 15:04"))
			return runner.ExitOK
		}
	}
	if *catchUp {
		missed, why := catchUpNeeded(*cfgPath, launchd.System().InstalledAt(), time.Now())
		fmt.Println(why)
		if !missed {
			return runner.ExitOK
		}
		// Au démarrage, le réseau n'est pas toujours prêt.
		time.Sleep(catchUpDelay)
	}

	if *force && len(jobs) != 1 {
		fmt.Fprintln(os.Stderr, "--force-deletes exige un seul --job : on ne lève le garde-fou que pour le job vérifié")
		return runner.ExitConfig
	}

	// Verrou d'abord : une erreur de config ne doit pas écraser l'état d'une exécution en cours.
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

	cfg, err := config.Load(*cfgPath, config.LoadOptions{})
	if err == nil {
		err = platformConfig(cfg)
	}
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
	awsEnv, err := awsKeysEnv(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		recordConfigError(err)
		return runner.ExitConfig
	}

	ignoreLogoff()
	sigCtx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()
	ctx, cancel := context.WithCancelCause(sigCtx)
	defer cancel(nil)

	// Icônes des sessions : préavis, fermeture douce et relance d'Outlook, annulation.
	// Seuls les comptes de tray.allow_run_now peuvent annuler : on compare le compte du
	// processus de l'icône (SID de son jeton) à ceux de la liste.
	if err := procs.EnableDebugPrivilege(); err != nil {
		fmt.Fprintln(os.Stderr, "icônes : SeDebugPrivilege indisponible :", err)
	}
	cancelSIDs := map[string]string{}
	for _, name := range cfg.Tray.AllowRunNow {
		if sid, err := setup.AccountSID(name); err == nil {
			cancelSIDs[sid] = name
		}
	}
	hub := ipc.NewHub(ipc.Dial, ipc.Msg{Version: version}, func(session int, sid string) {
		name, ok := cancelSIDs[sid]
		if sid == "" || !ok {
			fmt.Fprintf(os.Stderr, "annulation demandée depuis la session %d : refusée, compte hors de tray.allow_run_now\n", session)
			return
		}
		cancel(fmt.Errorf("annulée depuis l'icône par %s", name))
	})
	defer hub.Close()
	hub.Discover(ipc.Sessions)
	if dir := setup.ProgramDir(); dir != "" {
		if self, err := os.Executable(); err == nil && !strings.EqualFold(filepath.Dir(self), dir) {
			fmt.Fprintf(os.Stderr, "note : lancé hors de %s, run ne dialogue qu'avec les icônes lancées depuis son propre dossier ;\n"+
				"les icônes installées ne seront pas jointes (préavis par simple attente, pas de relance d'Outlook)\n", dir)
		}
	}

	var console io.Writer = os.Stdout
	if *quiet {
		console = nil
	}
	// Une destination de bilan mal configurée (secret absent…) ne doit pas empêcher la
	// sauvegarde : on le signale et on continue sans bilan.
	rep, err := report.New(cfg.Notify.Report, vault(), reportOptions(cfg))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bilan désactivé pour cette exécution :", err)
	}
	st, err := runner.Run(ctx, cfg, runner.Options{JobIDs: jobs, DryRun: *dryRun, ForceDeletes: *force}, runner.Deps{
		Exec:        execx.System{},
		Procs:       procs.System(),
		AWSCLI:      aws,
		StatusFile:  paths.StatusFile(),
		HistoryFile: paths.HistoryFile(),
		RunsDir:     paths.RunsDir(),
		Console:     console,
		Reporter:    rep,
		AWSEnv:      awsEnv,
		Trays:       hub,
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
		if awsinstall.IsSnap(configured) {
			return "", fmt.Errorf("profile.aws_cli : %q est la CLI AWS du paquet snap, qui ne démarre pas sous le service (droits restreints) : laisser profile.aws_cli vide et relancer sudo kura-agent install, qui installe la version officielle", configured)
		}
		return configured, nil
	}
	// Aussi à l'emplacement par défaut du MSI : juste après une installation par
	// kura-agent install, le PATH des processus déjà lancés n'en sait encore rien.
	p, err := awsinstall.Find()
	if err != nil {
		return "", errors.New("aws introuvable dans le PATH : installer AWS CLI v2 (ou relancer kura-agent install) ou renseigner profile.aws_cli")
	}
	return p, nil
}

// awsKeysEnv lit les clés d'accès désignées par profile.aws_credentials.
func awsKeysEnv(cfg *config.Config) ([]string, error) {
	if cfg.Profile.AWSCredentials == "" {
		return nil, nil
	}
	v, err := vault().Resolve(cfg.Profile.AWSCredentials)
	if err != nil {
		return nil, fmt.Errorf("profile.aws_credentials : %w", err)
	}
	k, err := creds.ParseAWSKeys(v)
	if err != nil {
		return nil, fmt.Errorf("profile.aws_credentials : %w", err)
	}
	return k.Env(), nil
}

func vault() creds.Store { return creds.System(paths.CredentialsDir()) }

func joinAWSKeys(id, secret string) (string, error) { return creds.JoinAWSKeys(id, secret) }

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
	if err := platformConfig(cfg); err != nil {
		check("fonctions demandées sous "+platform.OSName(), err)
	}
	aws, err := resolveAWS(cfg.Profile.AWSCLI)
	check("AWS CLI", err)
	if err == nil {
		out, err := exec.Command(aws, "--version").CombinedOutput()
		if err == nil {
			fmt.Printf("       %s\n", strings.TrimSpace(string(out)))
		}
		check("AWS CLI exécutable", err)
		keys, err := awsKeysEnv(cfg)
		if cfg.Profile.AWSCredentials != "" {
			check("clés d'accès "+cfg.Profile.AWSCredentials, err)
		}
		if !*offline && err == nil {
			cmd := exec.Command(aws, "s3api", "head-bucket", "--bucket", cfg.Profile.Bucket,
				"--endpoint-url", cfg.Profile.Endpoint)
			cmd.Env = append(append(os.Environ(), awscli.Env(cfg.Profile)...), keys...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				err = fmt.Errorf("%v : %s", err, strings.TrimSpace(string(out)))
			}
			via := cfg.Profile.AWSCredentials
			if via == "" {
				via = fmt.Sprintf("profil %q", cfg.Profile.AWSProfile)
			}
			check(fmt.Sprintf("accès au bucket %s (%s)", cfg.Profile.Bucket, via), err)
		}
	}
	if r := cfg.Notify.Report; r != nil {
		label := "bilan : " + r.Type
		if *offline {
			_, err := report.New(r, vault(), reportOptions(cfg))
			check(label+" (secrets résolus, pas d'envoi)", err)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			note, err := report.Check(ctx, r, vault(), reportOptions(cfg))
			cancel()
			check(label, err)
			if err == nil && note != "" {
				fmt.Printf("       %s\n", note)
			}
		}
	}
	if !ok {
		return runner.ExitConfig
	}
	return runner.ExitOK
}

func reportOptions(cfg *config.Config) report.Options {
	host, _ := os.Hostname()
	return report.Options{Version: version, Host: host, Profile: cfg.Profile.Name,
		Bucket: cfg.Profile.Bucket, PendingDir: paths.PendingDir()}
}

func cmdCredentials(args []string) int {
	store := vault()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "list":
		names, err := store.List()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if len(names) == 0 {
			fmt.Println("aucun secret enregistré")
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return 0
	case "delete":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage : kura-agent credentials delete NOM")
			return 2
		}
		if err := store.Delete(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Printf("secret %q supprimé\n", args[1])
		return 0
	case "set":
		fs := flag.NewFlagSet("credentials set", flag.ContinueOnError)
		fromEnv := fs.String("from-env", "", "lire la valeur dans cette variable d'environnement")
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			fmt.Fprintln(os.Stderr, "usage : kura-agent credentials set NOM [--from-env VARIABLE]")
			return 2
		}
		name := args[1]
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		var value string
		if *fromEnv != "" {
			value = os.Getenv(*fromEnv)
		} else {
			v, err := askSecret(fmt.Sprintf("valeur de %q (non affichée) : ", name))
			if err != nil {
				fmt.Fprintln(os.Stderr, "lecture impossible :", err)
				return 2
			}
			value = v
		}
		if err := store.Set(name, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Printf("secret %q enregistré ; référence dans la configuration : credential:%s\n", name, name)
		return 0
	case "set-aws":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage : kura-agent credentials set-aws NOM")
			return 2
		}
		value, err := askAWSKeys(args[1])
		if err == nil {
			err = store.Set(args[1], value)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Printf("clés %q enregistrées ; référence dans la configuration : aws_credentials: credential:%s\n", args[1], args[1])
		return 0
	default:
		fmt.Fprintf(os.Stderr, "sous-commande inconnue %q (set, set-aws, delete, list)\n", args[0])
		return 2
	}
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
	if st.State == "running" {
		fmt.Fprintf(w, "\nExécution %s : en cours depuis %s", st.RunID, st.StartedAt.Local().Format("15:04"))
	} else {
		fmt.Fprintf(w, "\nExécution %s : %s (code %d)", st.RunID, label(st.Result), st.ExitCode)
	}
	if st.DryRun {
		fmt.Fprint(w, " — simulation")
	}
	fmt.Fprintln(w)
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
