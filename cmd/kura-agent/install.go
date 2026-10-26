package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/schedtask"
	"github.com/E-C-G/kura-agent/internal/setup"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// system regroupe les opérations système d'install et d'uninstall, remplaçables
// dans les tests.
type system struct {
	checkAdmin       func() error
	secureDataDir    func(dir string) error
	installBinary    func(dir string) (exe, gui string, err error)
	setAutostart     func(gui string) error
	removeAutostart  func() error
	stopTrays        func() int
	programDir       func() string
	ensureAccount    func(name string) (sid string, created bool, err error)
	removeAccount    func(name string) error
	accountSID       func(name string) (string, error)
	validate         func(configFile string) int
	registerTask     func(xml string) error
	deleteTask       func() error
	removeProgramDir func(dir string) (pendingReboot bool, err error)
	acquireLock      func() (lock.Release, error)
	askSecret        func(prompt string) (string, error)
	askAWSKeys       func(name string) (string, error)
	now              func() time.Time

	vault      creds.Store
	dataDir    string
	configFile string
	out        io.Writer
}

func realSystem() system {
	return system{
		checkAdmin:       setup.CheckAdmin,
		secureDataDir:    setup.SecureDataDir,
		installBinary:    setup.InstallBinary,
		setAutostart:     setup.SetTrayAutostart,
		removeAutostart:  setup.RemoveTrayAutostart,
		stopTrays:        setup.StopTrays,
		programDir:       setup.ProgramDir,
		ensureAccount:    setup.EnsureAccount,
		removeAccount:    setup.RemoveAccount,
		accountSID:       setup.AccountSID,
		validate:         func(f string) int { return cmdValidate([]string{"--config", f}) },
		registerTask:     schedtask.Register,
		deleteTask:       schedtask.Delete,
		removeProgramDir: setup.RemoveProgramDir,
		acquireLock:      lock.Acquire,
		askSecret:        askSecret,
		askAWSKeys:       askAWSKeys,
		now:              time.Now,
		vault:            vault(),
		dataDir:          paths.DataDir(),
		configFile:       paths.ConfigFile(),
		out:              os.Stdout,
	}
}

const (
	exitInstallOK     = 0
	exitInstallFailed = 2
	exitInstallConfig = 3
)

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cfgPath := fs.String("config", paths.ConfigFile(), "configuration à installer (copiée dans "+paths.ConfigFile()+")")
	account := fs.String("account", setup.DefaultAccount, "compte local qui exécute la tâche")
	skipValidate := fs.Bool("skip-validate", false, "créer la tâche même si validate échoue (déconseillé)")
	if err := fs.Parse(args); err != nil {
		return exitInstallConfig
	}
	return install(realSystem(), *cfgPath, *account, *skipValidate)
}

func install(sys system, cfgPath, account string, skipValidate bool) int {
	out := sys.out
	step := func(format string, a ...any) { fmt.Fprintf(out, "• "+format+"\n", a...) }
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "ÉCHEC : "+format+"\n", a...)
		return code
	}

	if err := sys.checkAdmin(); err != nil {
		return fail(exitInstallFailed, "%v", err)
	}
	cfg, err := config.Load(cfgPath, config.LoadOptions{CheckSystem: true})
	if err != nil {
		return fail(exitInstallConfig, "%v", err)
	}
	if cfg.Profile.AWSCredentials == "" {
		fmt.Fprintf(out, "Attention : sans profile.aws_credentials, la CLI AWS cherchera ses clés dans le dossier .aws du compte %s,\n"+
			"qui n'existe pas. Conseillé : aws_credentials: credential:wasabi (clés saisies pendant l'installation).\n", account)
	}

	if err := sys.secureDataDir(sys.dataDir); err != nil {
		return fail(exitInstallFailed, "dossier %s : %v", sys.dataDir, err)
	}
	if err := sys.vault.Init(); err != nil {
		return fail(exitInstallFailed, "coffre %s : %v", sys.vault.Dir, err)
	}
	step("dossier %s protégé (Utilisateurs en lecture, coffre réservé aux administrateurs)", sys.dataDir)

	backup, err := installConfig(cfgPath, sys.configFile, sys.now())
	if err != nil {
		return fail(exitInstallFailed, "copie de la configuration : %v", err)
	}
	if backup != "" {
		step("configuration installée dans %s (ancienne version : %s)", sys.configFile, filepath.Base(backup))
	} else {
		step("configuration : %s", sys.configFile)
	}

	exe, gui, err := sys.installBinary(sys.programDir())
	if err != nil {
		return fail(exitInstallFailed, "copie de l'exécutable : %v", err)
	}
	step("exécutable : %s", exe)
	if gui == "" {
		fmt.Fprintf(out, "Attention : %s absent à côté de l'exécutable : pas d'icône (préavis par simple attente, pas de relance d'Outlook).\n", setup.GUIName)
	}

	sid, created, err := sys.ensureAccount(account)
	if err != nil {
		return fail(exitInstallFailed, "compte %s : %v", account, err)
	}
	if created {
		step("compte %s créé (Administrateurs, sans ouverture de session locale ni Bureau à distance, masqué de l'écran d'accueil)", account)
	} else {
		step("compte %s vérifié (droits et groupe réappliqués)", account)
	}

	for _, ref := range cfg.CredentialRefs() {
		if v, err := sys.vault.Get(ref.Name); err == nil {
			// Réécrit en portée machine : un secret de la v0.2.0, chiffré pour le compte
			// qui l'a saisi, serait illisible pour svc-kura.
			if err := sys.vault.Set(ref.Name, v); err != nil {
				return fail(exitInstallFailed, "secret %q : %v", ref.Name, err)
			}
			step("secret %q présent (%s)", ref.Name, ref.Field)
			continue
		} else if !errors.Is(err, creds.ErrNotFound) {
			return fail(exitInstallFailed, "secret %q : %v", ref.Name, err)
		}
		fmt.Fprintf(out, "Le secret %q (%s) n'est pas encore dans le coffre.\n", ref.Name, ref.Field)
		var value string
		if ref.AWS {
			value, err = sys.askAWSKeys(ref.Name)
		} else {
			value, err = sys.askSecret(fmt.Sprintf("%s (non affiché) : ", ref.Name))
		}
		if err == nil {
			err = sys.vault.Set(ref.Name, value)
		}
		if err != nil {
			hint := "kura-agent credentials set " + ref.Name
			if ref.AWS {
				hint = "kura-agent credentials set-aws " + ref.Name
			}
			return fail(exitInstallFailed, "secret %q non enregistré (%v) ; l'enregistrer avec « %s » puis relancer install", ref.Name, err, hint)
		}
		step("secret %q enregistré", ref.Name)
	}

	if !skipValidate {
		fmt.Fprintln(out, "\nVérification (validate) :")
		if code := sys.validate(sys.configFile); code != 0 {
			return fail(exitInstallFailed, "validate a échoué : tâche %s non créée ni modifiée. Corriger puis relancer install.", schedtask.Name)
		}
		fmt.Fprintln(out)
	}

	var runNow []string
	for _, u := range cfg.Tray.AllowRunNow {
		s, err := sys.accountSID(u)
		if err != nil {
			return fail(exitInstallConfig, "tray.allow_run_now : compte %q introuvable (%v)", u, err)
		}
		runNow = append(runNow, s)
	}
	def, err := schedtask.XML(cfg.Schedule, schedtask.Params{
		UserID: sid, Command: exe, Arguments: "run --quiet --scheduled", WorkingDir: sys.dataDir,
		RunNowSIDs: runNow, Now: sys.now(),
	})
	if err != nil {
		return fail(exitInstallConfig, "tâche planifiée : %v", err)
	}
	// Copie de la définition, sans secret, pour diagnostiquer un refus de schtasks.
	taskXML := filepath.Join(sys.dataDir, "task.xml")
	if err := os.WriteFile(taskXML, []byte(def), 0o644); err != nil {
		fmt.Fprintf(out, "Attention : %s non écrit (%v)\n", taskXML, err)
	}
	var partial *schedtask.RunNowNotAppliedError
	switch err := sys.registerTask(def); {
	case errors.As(err, &partial):
		fmt.Fprintf(out, "Attention : %v.\nSauvegarder maintenant ne fonctionnera que pour les administrateurs.\n", err)
	case err != nil:
		return fail(exitInstallFailed, "tâche planifiée : %v\n(définition : %s)", err, taskXML)
	}
	step("tâche %s enregistrée : %s, compte %s sans mot de passe stocké (S4U), autorisations maximales", schedtask.Name, describeSchedule(cfg.Schedule), account)

	if gui != "" {
		if err := sys.setAutostart(gui); err != nil {
			return fail(exitInstallFailed, "lancement de l'icône à l'ouverture de session : %v", err)
		}
		step("icône lancée à chaque ouverture de session pour %s", strings.Join(cfg.Tray.Users, ", "))
	}

	fmt.Fprintf(out, `
Installation terminée.
Suite conseillée :
  1. kura-agent run --dry-run          simulation, rien n'est copié ni supprimé
  2. schtasks /Run /TN %s        une vraie exécution sous %s, puis kura-agent status
  3. après une nuit sans erreur, désactiver puis supprimer l'ancienne tâche et son script
L'icône apparaît à la prochaine ouverture de session ; pour l'ouvrir tout de suite dans
une session déjà ouverte, y lancer « %s tray » (sans élévation).
`, schedtask.Name, account, filepath.Join(sys.programDir(), setup.GUIName))
	return exitInstallOK
}

// installConfig copie la configuration à son emplacement de production. Une version
// différente déjà en place est gardée à côté (config.yaml.AAAAMMJJ-HHMMSS.bak).
func installConfig(src, dst string, now time.Time) (backup string, err error) {
	if samePath(src, dst) {
		return "", nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	old, err := os.ReadFile(dst)
	switch {
	case err == nil:
		if bytes.Equal(old, data) {
			return "", nil
		}
		backup = dst + "." + now.Format("20060102-150405") + ".bak"
		if err := os.Rename(dst, backup); err != nil {
			return "", err
		}
	case !errors.Is(err, os.ErrNotExist):
		// Illisible mais présente : ne jamais l'écraser sans copie.
		return "", fmt.Errorf("lecture de %s : %w", dst, err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return backup, err
	}
	return backup, os.Rename(tmp, dst)
}

func samePath(a, b string) bool {
	ca, err1 := filepath.Abs(a)
	cb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	if os.PathSeparator == '\\' {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

var dayLabels = map[string]string{"mon": "lun", "tue": "mar", "wed": "mer", "thu": "jeu", "fri": "ven", "sat": "sam", "sun": "dim"}

func describeSchedule(s config.Schedule) string {
	var b strings.Builder
	switch s.Type {
	case "hourly":
		b.WriteString("toutes les heures")
		if s.Time != "" {
			b.WriteString(" à partir de " + s.Time)
		}
	default:
		b.WriteString("à " + s.Time)
	}
	if len(s.Days) > 0 {
		days := make([]string, len(s.Days))
		for i, d := range s.Days {
			days[i] = dayLabels[d]
		}
		b.WriteString(" (" + strings.Join(days, ", ") + ")")
	} else if s.Type != "weekly" {
		b.WriteString(" tous les jours")
	}
	if s.RunIfMissed {
		b.WriteString(", rattrapage si manquée")
	}
	return b.String()
}

func cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	account := fs.String("account", setup.DefaultAccount, "compte local créé par install")
	keepAccount := fs.Bool("keep-account", false, "garder le compte de service")
	purge := fs.Bool("purge", false, "supprimer aussi "+paths.DataDir()+" (configuration, secrets, journaux)")
	if err := fs.Parse(args); err != nil {
		return exitInstallConfig
	}
	return uninstall(realSystem(), *account, *keepAccount, *purge)
}

func uninstall(sys system, account string, keepAccount, purge bool) int {
	out := sys.out
	step := func(format string, a ...any) { fmt.Fprintf(out, "• "+format+"\n", a...) }
	if err := sys.checkAdmin(); err != nil {
		fmt.Fprintln(os.Stderr, "ÉCHEC :", err)
		return exitInstallFailed
	}
	release, err := sys.acquireLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ÉCHEC : une sauvegarde est en cours ; réessayer quand elle sera terminée (kura-agent status)")
		return exitInstallFailed
	}
	defer release()

	var errs []error
	if err := sys.removeAutostart(); err != nil {
		errs = append(errs, fmt.Errorf("lancement de l'icône : %w", err))
	}
	if n := sys.stopTrays(); n > 0 {
		step("%d icône(s) fermée(s)", n)
	}
	if err := sys.deleteTask(); err != nil {
		errs = append(errs, err)
	} else {
		step("tâche %s supprimée", schedtask.Name)
	}
	if !keepAccount {
		if err := sys.removeAccount(account); err != nil {
			errs = append(errs, fmt.Errorf("compte %s : %w", account, err))
		} else {
			step("compte %s et son profil supprimés", account)
		}
	}
	if dir := sys.programDir(); dir != "" {
		reboot, err := sys.removeProgramDir(dir)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s : %w", dir, err))
		case reboot:
			step("%s sera effacé au prochain redémarrage (exécutable en cours d'utilisation)", dir)
		default:
			step("%s supprimé", dir)
		}
	}
	if purge {
		if err := os.RemoveAll(sys.dataDir); err != nil {
			errs = append(errs, err)
		} else {
			step("%s supprimé (configuration, secrets, journaux)", sys.dataDir)
		}
	} else {
		step("%s conservé (configuration, secrets, journaux) ; --purge pour l'effacer", sys.dataDir)
	}
	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "ÉCHEC :", errors.Join(errs...))
		return exitInstallFailed
	}
	return exitInstallOK
}
