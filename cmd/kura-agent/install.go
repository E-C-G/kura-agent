package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/awsinstall"
	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/internal/launchd"
	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/schedtask"
	"github.com/E-C-G/kura-agent/internal/setup"
	"github.com/E-C-G/kura-agent/internal/systemd"
	"github.com/E-C-G/kura-agent/pkg/config"
	"github.com/E-C-G/kura-agent/pkg/pack"
)

// system regroupe les opérations système d'install et d'uninstall, remplaçables
// dans les tests.
type system struct {
	checkAdmin      func() error
	secureDataDir   func(dir string) error
	installBinary   func(dir string) (exe, gui string, err error)
	setAutostart    func(gui string) error
	removeAutostart func() error
	stopTrays       func() int
	programDir      func() string
	legacyAccount   func(name string) bool // compte svc-kura d'une version précédente
	removeAccount   func(name string) error
	accountSID      func(name string) (string, error)
	validate        func(configFile string) int
	registerTask    func(xml string) error
	deleteTask      func() error
	// Windows : tâche \Kura\Restore (restauration depuis l'icône).
	registerRestoreTask func(xml string) error
	deleteRestoreTask   func() error
	removeProgramDir    func(dir string) (pendingReboot bool, err error)
	acquireLock         func() (lock.Release, error)
	askSecret           func(prompt string) (string, error)
	askAWSKeys          func(name string) (string, error)
	// ensureAWS installe la CLI AWS si elle manque (MSI fourni, sinon téléchargé) ;
	// tempDir : dossier de travail réservé aux administrateurs.
	ensureAWS func(tempDir, want string, candidates []string) (awsinstall.Result, error)
	remove    func(path string) error
	now       func() time.Time

	// Linux : compte de service, timer systemd à la place de la tâche Windows,
	// exécutable copié dans /usr/local/bin.
	linux         bool
	ensureAccount func() (created bool, err error)
	installTimer  func(systemd.Units) error
	removeBinary  func() (string, error)

	// macOS : démons launchd sous root, exécutable dans /usr/local/bin, clé du coffre
	// dans le trousseau Système (retirée par uninstall --purge).
	darwin         bool
	installDaemons func(launchd.Plists) error
	removeVaultKey func() error
	logsDir        string

	vault      creds.Store
	dataDir    string
	configFile string
	awsWorkDir string   // dossier de travail de la CLI AWS, réservé aux administrateurs
	purgeDirs  []string // dossiers retirés par uninstall --purge, en plus de dataDir
	out        io.Writer
}

func realSystem() system {
	sys := system{
		checkAdmin:      setup.CheckAdmin,
		secureDataDir:   setup.SecureDataDir,
		installBinary:   setup.InstallBinary,
		setAutostart:    setup.SetTrayAutostart,
		removeAutostart: setup.RemoveTrayAutostart,
		stopTrays:       setup.StopTrays,
		programDir:      setup.ProgramDir,
		legacyAccount:   func(n string) bool { _, err := setup.LocalAccountSID(n); return err == nil },
		removeAccount:   setup.RemoveAccount,
		accountSID:      setup.AccountSID,
		validate:        func(f string) int { return cmdValidate([]string{"--config", f}) },
		registerTask:    schedtask.Register,
		deleteTask:      schedtask.Delete,
		registerRestoreTask: func(def string) error {
			return schedtask.RegisterNamed(schedtask.RestoreName, def)
		},
		deleteRestoreTask: func() error { return schedtask.DeleteNamed(schedtask.RestoreName) },
		removeProgramDir:  setup.RemoveProgramDir,
		acquireLock:       lock.Acquire,
		askSecret:         askSecret,
		askAWSKeys:        askAWSKeys,
		ensureAWS: func(dir, want string, c []string) (awsinstall.Result, error) {
			return awsinstall.System(os.Stdout, dir).Ensure(want, c)
		},
		remove:     os.Remove,
		now:        time.Now,
		vault:      vault(),
		dataDir:    paths.DataDir(),
		configFile: paths.ConfigFile(),
		awsWorkDir: filepath.Join(paths.DataDir(), "awscli"),
		out:        os.Stdout,
	}
	if runtime.GOOS == "linux" {
		timers := systemd.System()
		sys.linux = true
		sys.ensureAccount = setup.EnsureServiceAccount
		sys.installTimer = timers.Install
		sys.deleteTask = timers.Remove
		sys.removeBinary = setup.RemoveBinary
		// /var/lib/kura appartient au compte du service : le travail de la CLI AWS se
		// fait dans un dossier de root.
		sys.awsWorkDir = "/var/cache/kura-install"
		sys.purgeDirs = []string{paths.LogsDir(), paths.ConfigDir(), sys.awsWorkDir}
	}
	if runtime.GOOS == "darwin" {
		daemons := launchd.System()
		sys.darwin = true
		sys.installDaemons = daemons.Install
		sys.deleteTask = daemons.Remove
		sys.removeBinary = setup.RemoveBinary
		sys.removeVaultKey = creds.DeleteSystemKey
		sys.purgeDirs = []string{paths.LogsDir()}
	}
	sys.logsDir = paths.LogsDir()
	return sys
}

const (
	exitInstallOK     = 0
	exitInstallFailed = 2
	exitInstallConfig = 3
)

// installOptions : options de la commande install.
type installOptions struct {
	cfgPath      string
	skipValidate bool
	// pkgPath : paquet d'installation .kura (configuration et secrets) préparé par la
	// console ; remplace cfgPath.
	pkgPath string
	keep    bool // garder le .kura après une installation réussie
	// msiCandidates : emplacements possibles du paquet de la CLI AWS (AWSCLIV2.msi
	// sous Windows, zip et .sig sous Linux, AWSCLIV2.pkg sous macOS), si elle manque.
	msiCandidates []string
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cfgPath := fs.String("config", paths.ConfigFile(), "configuration à installer (copiée dans "+paths.ConfigFile()+")")
	pkg := fs.String("package", "", "paquet d'installation .kura préparé par la console (configuration et secrets)")
	keep := fs.Bool("keep", false, "garder le fichier .kura après l'installation")
	awsPkg := awsinstall.SystemPackage().Name
	msi := fs.String("aws-cli", "", "paquet de la CLI AWS à utiliser si elle manque, signature .sig à côté sous Linux (défaut : "+orNone(awsPkg)+" à côté du paquet ou de kura-agent, sinon téléchargé chez AWS)")
	fs.StringVar(msi, "aws-cli-msi", "", "ancien nom de --aws-cli")
	skipValidate := fs.Bool("skip-validate", false, "créer la tâche même si validate échoue (déconseillé)")
	if err := fs.Parse(args); err != nil {
		return exitInstallConfig
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if fs.NArg() != 0 || (explicit["config"] && explicit["package"]) {
		fmt.Fprintln(os.Stderr, "usage : kura-agent install [--config FICHIER | --package FICHIER.kura] [--aws-cli FICHIER] [--keep] [--skip-validate]")
		return exitInstallConfig
	}
	if *msi != "" {
		if st, err := os.Stat(*msi); err != nil || !st.Mode().IsRegular() {
			fmt.Fprintf(os.Stderr, "ÉCHEC : --aws-cli : %s introuvable\n", *msi)
			return exitInstallConfig
		}
	}
	opt := installOptions{cfgPath: *cfgPath, skipValidate: *skipValidate, pkgPath: *pkg, keep: *keep}
	opt.msiCandidates = []string{*msi}
	if *msi == "" {
		if *pkg != "" {
			opt.msiCandidates = append(opt.msiCandidates, filepath.Join(filepath.Dir(*pkg), awsPkg))
		}
		if exe, err := os.Executable(); err == nil {
			opt.msiCandidates = append(opt.msiCandidates, filepath.Join(filepath.Dir(exe), awsPkg))
		}
	}
	return install(realSystem(), opt)
}

// openInstallPackage ouvre un paquet d'installation (trois essais de mot de passe).
func openInstallPackage(sys system, file string) (pack.Contents, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return pack.Contents{}, err
	}
	var c pack.Contents
	for try := 1; ; try++ {
		pw, err := sys.askSecret("mot de passe du paquet (affiché par la console) : ")
		if err != nil {
			return c, fmt.Errorf("lecture du mot de passe : %w", err)
		}
		c, err = pack.Open(data, pw)
		if err == nil {
			break
		}
		if !errors.Is(err, pack.ErrWrongPassword) || try == 3 {
			return c, err
		}
		fmt.Fprintln(sys.out, "mot de passe incorrect")
	}
	switch {
	case c.Kind == pack.KindCredentials:
		return c, errors.New("ce paquet n'apporte qu'une clé : l'importer avec « kura-agent import " + file + " »")
	case c.Kind != pack.KindInstall:
		return c, fmt.Errorf("paquet de type %q : pas un paquet d'installation", c.Kind)
	case c.Expired(sys.now()):
		return c, fmt.Errorf("paquet expiré depuis le %s : en générer un nouveau dans la console", c.ExpiresAt.Local().Format("02/01/2006 15:04"))
	}
	return c, nil
}

// La tâche s'exécute sous SYSTEM : Windows refuse, sur certains postes, une tâche sans
// mot de passe (S4U) pour un autre compte que celui qui l'enregistre, et un compte
// dédié administrateur n'apportait pas de séparation réelle face à SYSTEM.
func install(sys system, opt installOptions) int {
	out := sys.out
	cfgPath, skipValidate := opt.cfgPath, opt.skipValidate
	installed := false // vrai une fois tout réussi (sinon les secrets du paquet sont retirés)
	var err error
	step := func(format string, a ...any) { fmt.Fprintf(out, "• "+format+"\n", a...) }
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "ÉCHEC : "+format+"\n", a...)
		return code
	}

	if err := sys.checkAdmin(); err != nil {
		return fail(exitInstallFailed, "%v", err)
	}
	var pkg *pack.Contents
	if opt.pkgPath != "" {
		c, err := openInstallPackage(sys, opt.pkgPath)
		if err != nil {
			return fail(exitInstallConfig, "%v", err)
		}
		pkg = &c
		fmt.Fprintf(out, "Paquet d'installation du %s pour %s / %s, bucket %s\n",
			c.CreatedAt.Local().Format("02/01/2006 15:04"), c.Client, c.Host, c.Bucket)
	}
	// La configuration est lue une seule fois : le texte vérifié est celui qui est
	// installé, même si le fichier source change pendant l'installation.
	var cfgData []byte
	if pkg != nil {
		cfgData, cfgPath = []byte(pkg.Config), ""
	} else if cfgData, err = os.ReadFile(cfgPath); err != nil {
		return fail(exitInstallConfig, "lecture de la configuration : %v", err)
	}
	cfg, err := config.Parse(cfgData, config.LoadOptions{CheckSystem: true, StagingDir: paths.StagingDir()})
	if err != nil {
		if pkg != nil {
			return fail(exitInstallConfig, "configuration du paquet : %v", err)
		}
		return fail(exitInstallConfig, "%v", err)
	}
	for _, t := range checkPrepareTools(cfg) {
		if t.Err != nil {
			return fail(exitInstallConfig, "%s : %v", t.Field, t.Err)
		}
		step("%s : %s (version %s)", t.Field, t.Path, t.Version)
	}
	if pkg != nil {
		if cfg.Profile.Bucket != pkg.Bucket {
			return fail(exitInstallConfig, "paquet incohérent : bucket %s, configuration %s", pkg.Bucket, cfg.Profile.Bucket)
		}
		provided := map[string]bool{}
		for _, cr := range pkg.Credentials {
			provided[cr.Name] = true
		}
		if ref := strings.TrimPrefix(cfg.Profile.AWSCredentials, "credential:"); ref == cfg.Profile.AWSCredentials || !provided[ref] {
			return fail(exitInstallConfig, "paquet incohérent : profile.aws_credentials (%s) ne désigne pas la clé du paquet", orNone(cfg.Profile.AWSCredentials))
		}
	}

	if cfg.Profile.AWSCredentials == "" {
		home := "le dossier .aws de SYSTEM\n(C:\\Windows\\System32\\config\\systemprofile)"
		if sys.linux {
			home = "/var/lib/kura/.aws"
		} else if sys.darwin {
			home = filepath.Join(sys.dataDir, ".aws")
		}
		fmt.Fprintf(out, "Attention : sans profile.aws_credentials, la CLI AWS cherchera ses clés dans %s. Conseillé : aws_credentials: credential:wasabi.\n", home)
	}

	if sys.linux {
		created, err := sys.ensureAccount()
		if err != nil {
			return fail(exitInstallFailed, "compte %s : %v", setup.ServiceAccount, err)
		}
		if created {
			step("compte de service %s créé (système, sans mot de passe ni shell)", setup.ServiceAccount)
		} else {
			step("compte de service %s présent", setup.ServiceAccount)
		}
	}
	if err := sys.secureDataDir(sys.dataDir); err != nil {
		return fail(exitInstallFailed, "dossier %s : %v", sys.dataDir, err)
	}
	if err := sys.vault.Init(); err != nil {
		return fail(exitInstallFailed, "coffre %s : %v", sys.vault.Dir, err)
	}
	if sys.linux {
		step("dossiers %s et %s prêts ; coffre %s réservé à root", sys.dataDir, filepath.Dir(sys.configFile), sys.vault.Dir)
	} else if sys.darwin {
		step("dossiers %s et %s prêts ; coffre %s réservé à root, clé dans le trousseau Système", sys.dataDir, sys.logsDir, sys.vault.Dir)
	} else {
		step("dossier %s protégé (Utilisateurs en lecture, coffre réservé aux administrateurs)", sys.dataDir)
	}

	// La CLI AWS avant la suite : sans elle, ni validate ni la sauvegarde ne
	// fonctionnent. Installée si profile.aws_cli est vide ou désigne l'emplacement du
	// MSI ; un autre chemin explicite n'est jamais installé. Le MSI est copié dans le
	// dossier de données, déjà protégé : seuls les administrateurs peuvent le modifier
	// entre la vérification de sa signature et msiexec.
	if def := awsinstall.DefaultPath(); cfg.Profile.AWSCLI == "" || (def != "" && samePath(cfg.Profile.AWSCLI, def)) {
		res, err := sys.ensureAWS(sys.awsWorkDir, cfg.Profile.AWSCLI, opt.msiCandidates)
		switch {
		case err != nil:
			return fail(exitInstallFailed, "%v", err)
		case res.Warning != "":
			fmt.Fprintf(out, "Attention : CLI AWS : %s\n", res.Warning)
		case res.Installed:
			step("CLI AWS installée : %s (%s, paquet signé par %s)", res.Version, res.Path, res.Signer)
			if res.RebootRequired {
				fmt.Fprintln(out, "Attention : Windows demande un redémarrage pour terminer l'installation de la CLI AWS.")
			}
		default:
			step("CLI AWS : %s", res.Version)
		}
	}

	if pkg != nil {
		// Les secrets du paquet remplacent ceux du coffre : c'est la configuration
		// préparée pour ce poste. Les anciennes valeurs sont remises si l'installation
		// échoue ensuite (clé pas encore active, validate en échec…).
		previous := map[string]*string{}
		for _, cr := range pkg.Credentials {
			if v, err := sys.vault.Get(cr.Name); err == nil {
				previous[cr.Name] = &v
			} else if errors.Is(err, creds.ErrNotFound) {
				previous[cr.Name] = nil
			} else {
				return fail(exitInstallFailed, "secret %q : %v", cr.Name, err)
			}
		}
		defer func() {
			if installed {
				return
			}
			for name, v := range previous {
				if v != nil {
					_ = sys.vault.Set(name, *v)
				} else {
					_ = sys.vault.Delete(name)
				}
			}
			fmt.Fprintln(out, "Secrets du coffre remis comme avant l'installation.")
		}()
		for _, cr := range pkg.Credentials {
			value := cr.Value
			if cr.Kind == "aws" {
				if value, err = creds.JoinAWSKeys(cr.AccessKeyID, cr.SecretAccessKey); err != nil {
					return fail(exitInstallFailed, "secret %q : %v", cr.Name, err)
				}
			}
			if err := sys.vault.Set(cr.Name, value); err != nil {
				return fail(exitInstallFailed, "secret %q : %v", cr.Name, err)
			}
		}
		step("%d secret(s) du paquet enregistré(s) dans le coffre", len(pkg.Credentials))
	}

	backup, err := installConfigData(cfgData, cfgPath, sys.configFile, sys.now())
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
	if gui == "" && !sys.linux && !sys.darwin {
		fmt.Fprintf(out, "Attention : %s absent à côté de l'exécutable : pas d'icône (préavis par simple attente, pas de relance d'Outlook).\n", setup.GUIName)
	}

	for _, ref := range cfg.CredentialRefs() {
		if v, err := sys.vault.Get(ref.Name); err == nil {
			// Réécrit en portée machine : un secret de la v0.2.0, chiffré pour le compte
			// qui l'a saisi, serait illisible pour la tâche.
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
			name := schedtask.Name
			if sys.linux {
				name = systemd.TimerName
			} else if sys.darwin {
				name = launchd.Label
			}
			return fail(exitInstallFailed, "validate a échoué : planification %s non créée ni modifiée. Corriger puis relancer install.", name)
		}
		fmt.Fprintln(out)
	}

	if sys.linux {
		if code := installLinuxTimer(sys, cfg, exe, step, fail); code != exitInstallOK {
			return code
		}
	} else if sys.darwin {
		if code := installMacDaemons(sys, cfg, exe, step, fail); code != exitInstallOK {
			return code
		}
	} else if code := installWindowsTask(sys, cfg, exe, step, fail); code != exitInstallOK {
		return code
	}

	if gui != "" {
		if err := sys.setAutostart(gui); err != nil {
			return fail(exitInstallFailed, "lancement de l'icône à l'ouverture de session : %v", err)
		}
		step("icône lancée à chaque ouverture de session pour %s", strings.Join(cfg.Tray.Users, ", "))
	}

	if pkg != nil && !opt.keep {
		if err := sys.remove(opt.pkgPath); err == nil {
			step("paquet %s supprimé (il contenait la clé du poste)", opt.pkgPath)
		} else {
			fmt.Fprintf(out, "Attention : paquet %s non supprimé (%v) : le supprimer, il contient la clé du poste.\n", opt.pkgPath, err)
		}
	}
	if pkg != nil {
		fmt.Fprintln(out, "\nUne fois la première sauvegarde réussie, confirmer l'installation de la clé dans la console.")
	}

	installed = true
	if sys.linux {
		fmt.Fprintf(out, `
Installation terminée.
Suite conseillée :
  1. sudo kura-agent run --dry-run           simulation, rien n'est copié ni supprimé
  2. sudo systemctl start --no-block %s   une vraie exécution, comme planifiée, puis kura-agent status
  3. systemctl list-timers %s      prochain passage ; journal : journalctl -u %s
`, systemd.ServiceName, systemd.TimerName, systemd.ServiceName)
		return exitInstallOK
	}
	if sys.darwin {
		fmt.Fprintf(out, `
Installation terminée.
Avant la première sauvegarde : Réglages Système > Confidentialité et sécurité > Accès
complet au disque, ajouter %s (sinon Documents, Bureau et Mail restent illisibles).
Suite conseillée :
  1. sudo kura-agent run --dry-run                  simulation, rien n'est copié ni supprimé
  2. sudo launchctl kickstart system/%s   une vraie exécution, comme planifiée, puis kura-agent status
  3. journaux : %s
`, exe, launchd.Label, sys.logsDir)
		return exitInstallOK
	}
	fmt.Fprintf(out, `
Installation terminée.
Suite conseillée :
  1. kura-agent run --dry-run          simulation, rien n'est copié ni supprimé
  2. schtasks /Run /TN %s        une vraie exécution, comme planifiée, puis kura-agent status
  3. après une nuit sans erreur, désactiver puis supprimer l'ancienne tâche et son script
L'icône apparaît à la prochaine ouverture de session ; pour l'ouvrir tout de suite dans
une session déjà ouverte, y lancer « %s tray » (sans élévation).
`, schedtask.Name, filepath.Join(sys.programDir(), setup.GUIName))
	return exitInstallOK
}

// installWindowsTask enregistre la tâche \Kura\Sync (SYSTEM) et retire l'ancien compte
// svc-kura.
func installWindowsTask(sys system, cfg *config.Config, exe string, step func(string, ...any), fail func(int, string, ...any) int) int {
	out := sys.out
	var runNow []string
	for _, u := range cfg.Tray.AllowRunNow {
		s, err := sys.accountSID(u)
		if err != nil {
			return fail(exitInstallConfig, "tray.allow_run_now : compte %q introuvable (%v)", u, err)
		}
		runNow = append(runNow, s)
	}
	def, err := schedtask.XML(cfg.Schedule, schedtask.Params{
		UserID: schedtask.SystemSID, Command: exe, Arguments: "run --quiet --scheduled", WorkingDir: sys.dataDir,
		RunNowSIDs: runNow, Now: sys.now(),
	})
	if err != nil {
		return fail(exitInstallConfig, "tâche planifiée : %v", err)
	}
	// Copie de la définition, sans secret, pour diagnostiquer un refus de schtasks ;
	// même encodage que le fichier passé à schtasks (UTF-16), donc réutilisable tel quel.
	taskXML := filepath.Join(sys.dataDir, "task.xml")
	if err := os.WriteFile(taskXML, schedtask.UTF16(def), 0o644); err != nil {
		fmt.Fprintf(out, "Attention : %s non écrit (%v)\n", taskXML, err)
	}
	var partial *schedtask.RunNowNotAppliedError
	switch err := sys.registerTask(def); {
	case errors.As(err, &partial):
		fmt.Fprintf(out, "Attention : %v.\nSauvegarder maintenant ne fonctionnera que pour les administrateurs.\n", err)
	case err != nil:
		return fail(exitInstallFailed, "tâche planifiée : %v\n(définition : %s)", err, taskXML)
	}
	step("tâche %s enregistrée : %s, compte SYSTEM, autorisations maximales", schedtask.Name, describeSchedule(cfg.Schedule))

	// Restauration depuis l'icône : tâche lançable par les comptes de tray.users.
	if len(cfg.Tray.Users) == 0 {
		if err := sys.deleteRestoreTask(); err != nil {
			fmt.Fprintf(out, "Attention : tâche %s non supprimée (%v)\n", schedtask.RestoreName, err)
		}
	} else {
		var users []string
		for _, u := range cfg.Tray.Users {
			s, err := sys.accountSID(u)
			if err != nil {
				return fail(exitInstallConfig, "tray.users : compte %q introuvable (%v)", u, err)
			}
			users = append(users, s)
		}
		def, err := schedtask.RestoreXML(schedtask.Params{Command: exe, Arguments: "restore --serve", WorkingDir: sys.dataDir, RunNowSIDs: users})
		if err != nil {
			return fail(exitInstallConfig, "tâche %s : %v", schedtask.RestoreName, err)
		}
		switch err := sys.registerRestoreTask(def); {
		case errors.As(err, &partial):
			fmt.Fprintf(out, "Attention : %v.\nRestaurer mes fichiers (icône) ne fonctionnera que pour les administrateurs.\n", err)
		case err != nil:
			return fail(exitInstallFailed, "tâche %s : %v", schedtask.RestoreName, err)
		default:
			step("tâche %s enregistrée : restauration depuis l'icône pour %s", schedtask.RestoreName, strings.Join(cfg.Tray.Users, ", "))
		}
	}

	// Compte de service des versions 0.3 à 0.4.1 : la tâche ne l'utilise plus.
	if sys.legacyAccount(setup.LegacyAccount) {
		if err := sys.removeAccount(setup.LegacyAccount); err != nil {
			fmt.Fprintf(out, "Attention : ancien compte %s non supprimé (%v)\n", setup.LegacyAccount, err)
		} else {
			step("ancien compte %s supprimé (la tâche tourne désormais sous SYSTEM)", setup.LegacyAccount)
		}
	}

	return exitInstallOK
}

// installLinuxTimer met en place kura-sync.service et kura-sync.timer (compte
// kura-agent).
func installLinuxTimer(sys system, cfg *config.Config, exe string, step func(string, ...any), fail func(int, string, ...any) int) int {
	if len(cfg.Tray.AllowRunNow) > 0 {
		fmt.Fprintln(sys.out, "Attention : tray.allow_run_now ignoré sous Linux pour l'instant (lancement : sudo systemctl start "+systemd.ServiceName+").")
	}
	withDocker := needsDockerGroup(cfg)
	if withDocker {
		if _, err := user.LookupGroup("docker"); err != nil {
			return fail(exitInstallConfig, "docker_volume avec pause : groupe docker introuvable (Docker Engine installé ?) ; ou pause: false")
		}
		step("socket Docker : groupe docker donné au seul service %s (gel des conteneurs ; équivaut aux droits root)", systemd.ServiceName)
	}
	units, err := systemd.Generate(cfg.Schedule, systemd.Params{Command: exe, User: setup.ServiceAccount, Docker: withDocker})
	if err != nil {
		return fail(exitInstallConfig, "planification : %v", err)
	}
	if err := sys.installTimer(units); err != nil {
		return fail(exitInstallFailed, "planification : %v", err)
	}
	step("timer %s actif : %s, compte %s", systemd.TimerName, describeSchedule(cfg.Schedule), setup.ServiceAccount)
	return exitInstallOK
}

// installMacDaemons met en place les démons launchd (root) : sauvegarde planifiée et,
// avec run_if_missed, rattrapage au démarrage.
func installMacDaemons(sys system, cfg *config.Config, exe string, step func(string, ...any), fail func(int, string, ...any) int) int {
	if len(cfg.Tray.AllowRunNow) > 0 {
		fmt.Fprintln(sys.out, "Attention : tray.allow_run_now ignoré sous macOS pour l'instant (lancement : sudo launchctl kickstart system/"+launchd.Label+").")
	}
	p, err := launchd.Generate(cfg.Schedule, launchd.Params{Command: exe, DataDir: sys.dataDir, LogsDir: sys.logsDir})
	if err != nil {
		return fail(exitInstallConfig, "planification : %v", err)
	}
	if err := sys.installDaemons(p); err != nil {
		return fail(exitInstallFailed, "planification : %v", err)
	}
	step("démon %s actif : %s, compte root", launchd.Label, describeSchedule(cfg.Schedule))
	if p.CatchUp != nil {
		step("rattrapage au démarrage : démon %s", launchd.CatchUpLabel)
	}
	return exitInstallOK
}

// installConfig copie la configuration à son emplacement de production. Une version
// différente déjà en place est gardée à côté (config.yaml.AAAAMMJJ-HHMMSS.bak).
func installConfig(src, dst string, now time.Time) (backup string, err error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	return installConfigData(data, src, dst, now)
}

// installConfigData installe le contenu data (lu dans src, vide s'il vient d'un paquet).
func installConfigData(data []byte, src, dst string, now time.Time) (backup string, err error) {
	if src != "" && samePath(src, dst) {
		return "", nil
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
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return backup, err
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
	purge := fs.Bool("purge", false, "supprimer aussi "+paths.DataDir()+" (configuration, secrets, journaux)")
	if err := fs.Parse(args); err != nil {
		return exitInstallConfig
	}
	return uninstall(realSystem(), *purge)
}

func uninstall(sys system, purge bool) int {
	out := sys.out
	step := func(format string, a ...any) { fmt.Fprintf(out, "• "+format+"\n", a...) }
	if err := sys.checkAdmin(); err != nil {
		fmt.Fprintln(os.Stderr, "ÉCHEC :", err)
		return exitInstallFailed
	}
	release, err := sys.acquireLock()
	if errors.Is(err, lock.ErrBusy) {
		fmt.Fprintln(os.Stderr, "ÉCHEC : une sauvegarde est en cours ; réessayer quand elle sera terminée (kura-agent status)")
		return exitInstallFailed
	}
	if err != nil {
		// Dossier de données déjà absent : rien ne peut tourner.
		release = func() {}
	}
	defer func() {
		if release != nil {
			release()
		}
	}()

	var errs []error
	if err := sys.removeAutostart(); err != nil {
		errs = append(errs, fmt.Errorf("lancement de l'icône : %w", err))
	}
	if n := sys.stopTrays(); n > 0 {
		step("%d icône(s) fermée(s)", n)
	}
	if err := sys.deleteTask(); err != nil {
		errs = append(errs, err)
	} else if sys.linux {
		step("timer %s et service %s retirés", systemd.TimerName, systemd.ServiceName)
	} else if sys.darwin {
		step("démons %s et %s retirés", launchd.Label, launchd.CatchUpLabel)
	} else {
		step("tâche %s supprimée", schedtask.Name)
		if err := sys.deleteRestoreTask(); err != nil {
			errs = append(errs, err)
		} else {
			step("tâche %s supprimée", schedtask.RestoreName)
		}
	}
	if sys.legacyAccount(setup.LegacyAccount) {
		if err := sys.removeAccount(setup.LegacyAccount); err != nil {
			errs = append(errs, fmt.Errorf("compte %s : %w", setup.LegacyAccount, err))
		} else {
			step("ancien compte %s et son profil supprimés", setup.LegacyAccount)
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
	if sys.linux || sys.darwin {
		switch p, err := sys.removeBinary(); {
		case err != nil:
			errs = append(errs, err)
		case p != "":
			step("%s supprimé", p)
		}
	}
	if purge {
		// Le verrou est un fichier du dossier de données : libéré avant de l'effacer (la
		// planification est déjà retirée).
		release()
		release = nil
		// Les secrets d'abord : sous Linux, ils ne sont pas dans dataDir.
		names, err := sys.vault.List()
		for _, n := range names {
			if err == nil {
				err = sys.vault.Delete(n)
			}
		}
		if err == nil && sys.removeVaultKey != nil {
			if err = sys.removeVaultKey(); err == nil {
				step("clé du coffre retirée du trousseau Système")
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("coffre : %w", err))
		}
		removed := []string{}
		for _, d := range append([]string{sys.dataDir}, sys.purgeDirs...) {
			if err := os.RemoveAll(d); err != nil {
				errs = append(errs, err)
			} else {
				removed = append(removed, d)
			}
		}
		if len(names) > 0 && err == nil {
			step("%d secret(s) supprimé(s) du coffre", len(names))
		}
		step("supprimé : %s (configuration, secrets, journaux)", strings.Join(removed, ", "))
	} else {
		step("%s conservé (configuration, secrets, journaux) ; --purge pour l'effacer", strings.Join(append([]string{sys.dataDir}, sys.purgeDirs...), ", "))
	}
	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "ÉCHEC :", errors.Join(errs...))
		return exitInstallFailed
	}
	return exitInstallOK
}
