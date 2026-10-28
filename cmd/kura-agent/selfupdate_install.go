package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/selfupdate"
	"github.com/E-C-G/kura-agent/internal/setup"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Codes de sortie de self-update (suite).
const (
	exitUpdateRolledBack = 6 // nouvelle version en échec, ancienne remise
	exitUpdateBroken     = 7 // retour arrière impossible : intervention requise
)

// minSelfUpdateTarget : première version qui sait terminer une mise à jour
// (self-update --finish) ; une plus ancienne serait installée puis aussitôt défaite.
const minSelfUpdateTarget = "0.10.0"

// installation : où l'agent est installé sur ce poste.
type installation struct {
	Mode    string   // selfupdate.ModeFiles ou ModeDeb
	Exe     string   // exécutable principal installé
	Files   []string // exécutables remplacés (Windows : avec kura-agentw.exe s'il est là)
	Staging string   // dossier de travail réservé aux administrateurs
}

// locateInstallation : les exécutables installés, pas celui qui tourne (une version
// téléchargée peut mettre à jour l'agent installé).
func locateInstallation() (installation, error) {
	exists := func(p string) bool { st, err := os.Stat(p); return err == nil && st.Mode().IsRegular() }
	switch runtime.GOOS {
	case "windows":
		dir := setup.ProgramDir()
		in := installation{Mode: selfupdate.ModeFiles, Exe: filepath.Join(dir, "kura-agent.exe"), Staging: filepath.Join(dir, ".update")}
		if !exists(in.Exe) {
			return in, fmt.Errorf("%s absent : agent non installé (kura-agent install)", in.Exe)
		}
		in.Files = []string{in.Exe}
		if gui := filepath.Join(dir, setup.GUIName); exists(gui) {
			in.Files = append(in.Files, gui)
		}
		return in, nil
	case "linux":
		in := installation{Staging: "/var/cache/kura-update"}
		if exists("/usr/bin/kura-agent") && exists("/var/lib/dpkg/info/kura-agent.list") {
			in.Mode, in.Exe = selfupdate.ModeDeb, "/usr/bin/kura-agent"
		} else if p := filepath.Join(setup.BinDir, "kura-agent"); exists(p) {
			in.Mode, in.Exe = selfupdate.ModeFiles, p
		} else {
			return in, errors.New("kura-agent n'est installé ni par son paquet (/usr/bin) ni dans " + setup.BinDir)
		}
		in.Files = []string{in.Exe}
		return in, nil
	case "darwin":
		in := installation{Mode: selfupdate.ModeFiles, Exe: filepath.Join(setup.BinDir, "kura-agent"), Staging: filepath.Join(paths.DataDir(), "update")}
		if !exists(in.Exe) {
			return in, fmt.Errorf("%s absent : agent non installé (sudo kura-agent install)", in.Exe)
		}
		in.Files = []string{in.Exe}
		return in, nil
	}
	return installation{}, fmt.Errorf("mise à jour non prise en charge sous %s", runtime.GOOS)
}

// updater : opérations de self-update, remplaçables dans les tests.
type updater struct {
	out         io.Writer
	checkAdmin  func() error
	locate      func() (installation, error)
	acquireLock func() (lock.Release, error)
	check       func(context.Context, selfupdate.Options) (selfupdate.Result, error)
	download    func(context.Context, selfupdate.Asset, string) error
	statusFile  string
	logsDir     string
	exeVersion  func(exe string) (string, error)
	finish      func(exe string) error
	dpkgInstall func(deb string) error
	debExtract  func(deb, dir string) (string, error)
	freeSpace   func(dir string) (uint64, error)
	interactive bool
	confirm     func(prompt string) (bool, error)
	goos        string
	goarch      string
	now         func() time.Time
}

func realUpdater(client *selfupdate.Client, out io.Writer) *updater {
	u := &updater{
		out:         out,
		checkAdmin:  setup.CheckAdmin,
		locate:      locateInstallation,
		acquireLock: lock.Acquire,
		statusFile:  updateStatusFile(),
		logsDir:     paths.LogsDir(),
		exeVersion:  exeVersion,
		finish:      runFinish,
		dpkgInstall: dpkgInstall,
		debExtract:  debExtract,
		freeSpace:   selfupdate.FreeSpace,
		interactive: stdinIsTerminal(),
		confirm: func(prompt string) (bool, error) {
			fmt.Fprint(os.Stderr, prompt)
			line, err := readLine()
			if err != nil {
				return false, err
			}
			a := strings.ToLower(strings.TrimSpace(line))
			return a == "o" || a == "oui" || a == "y" || a == "yes", nil
		},
		goos:   runtime.GOOS,
		goarch: runtime.GOARCH,
		now:    time.Now,
	}
	if client != nil {
		u.check, u.download = client.Check, client.Download
	}
	return u
}

func updateStatusFile() string { return filepath.Join(paths.DataDir(), "update.json") }

var versionLine = regexp.MustCompile(`^kura-agent (\S+) `)

// exeVersion lance « exe version » et lit la version annoncée.
func exeVersion(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "version").Output()
	if err != nil {
		return "", fmt.Errorf("%s version : %w", exe, err)
	}
	m := versionLine.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", fmt.Errorf("%s version : réponse inattendue %q", exe, strings.TrimSpace(string(out)))
	}
	return m[1], nil
}

// runFinish lance la finalisation par la nouvelle version.
func runFinish(exe string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "self-update", "--finish").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s : %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func dpkgInstall(deb string) error {
	cmd := exec.Command("dpkg", "-i", deb)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dpkg -i : %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// debExtract sort l'exécutable du paquet (dpkg-deb, présent sur tout Ubuntu).
func debExtract(deb, dir string) (string, error) {
	root := filepath.Join(dir, "deb")
	if out, err := exec.Command("dpkg-deb", "-x", deb, root).CombinedOutput(); err != nil {
		return "", fmt.Errorf("dpkg-deb -x : %w\n%s", err, strings.TrimSpace(string(out)))
	}
	exe := filepath.Join(root, "usr", "bin", "kura-agent")
	if st, err := os.Lstat(exe); err != nil || !st.Mode().IsRegular() {
		return "", errors.New("paquet sans usr/bin/kura-agent")
	}
	return exe, nil
}

// updateOptions : options de self-update (installation).
type updateOptions struct {
	tag      string
	yes      bool
	rollback bool
}

// logTo duplique la sortie dans update-<date>.log.
func (u *updater) logTo() (io.Writer, func()) {
	if err := os.MkdirAll(u.logsDir, 0o755); err != nil {
		return u.out, func() {}
	}
	f, err := os.OpenFile(filepath.Join(u.logsDir, "update-"+u.now().Format("20060102-150405")+".log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return u.out, func() {}
	}
	return io.MultiWriter(u.out, f), func() { f.Close() }
}

func (u *updater) readStatus() selfupdate.Status {
	s, err := selfupdate.ReadStatus(u.statusFile)
	if err != nil {
		fmt.Fprintf(u.out, "Attention : %v (remplacé)\n", err)
	}
	return s
}

func (u *updater) writeStatus(s selfupdate.Status) {
	if err := state.WriteAtomic(u.statusFile, s); err != nil {
		fmt.Fprintf(u.out, "Attention : %s non écrit : %v\n", u.statusFile, err)
	}
}

// run mène une mise à jour (ou un retour arrière) de bout en bout.
func (u *updater) run(ctx context.Context, o updateOptions) int {
	out, closeLog := u.logTo()
	defer closeLog()
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	refuse := func(format string, a ...any) int { say("Refusé : "+format, a...); return exitUpdateRefused }

	if err := u.checkAdmin(); err != nil {
		return refuse("%v", err)
	}
	inst, err := u.locate()
	if err != nil {
		return refuse("%v", err)
	}
	release, err := u.acquireLock()
	if errors.Is(err, lock.ErrBusy) {
		return refuse("une sauvegarde est en cours ; réessayer après sa fin")
	}
	if err != nil {
		return refuse("verrou : %v", err)
	}
	defer release()
	u.recover(inst, out)

	from, err := u.exeVersion(inst.Exe)
	if err != nil {
		return refuse("version installée illisible : %v", err)
	}
	if _, err := selfupdate.ParseVersion(from); err != nil {
		return refuse("version installée %q : mise à jour impossible (version de développement ?)", from)
	}
	if o.rollback {
		return u.rollback(ctx, inst, from, out)
	}

	res, err := u.check(ctx, selfupdate.Options{Current: from, Tag: o.tag, GOOS: u.goos, GOARCH: u.goarch})
	if err != nil {
		say("Échec : %v", err)
		return exitUpdateFailed
	}
	to := res.Latest.Version
	if cmp, _ := selfupdate.Compare(to, minSelfUpdateTarget); cmp < 0 {
		return refuse("%s ne sait pas terminer une mise à jour (self-update --finish) : version %s au moins", to, minSelfUpdateTarget)
	}
	switch cmp, _ := selfupdate.Compare(to, from); {
	case cmp == 0:
		say("kura-agent %s : déjà à jour.", from)
		return exitUpdateOK
	case cmp < 0:
		return refuse("%s est plus ancienne que la version installée %s (retour à la version précédente : --rollback)", to, from)
	}
	say("Mise à jour de kura-agent %s vers %s (%s)", from, to, inst.Exe)
	say("Archive : %s, attestation de release GitHub vérifiée", res.Asset.Name)
	say("Notes de version : %s", res.Latest.NotesURL)

	if free, err := u.freeSpace(filepath.Dir(inst.Exe)); err == nil && free < uint64(3*res.Asset.Size) {
		return refuse("espace disque insuffisant sur le volume de %s (%d Mo libres, %d Mo nécessaires)",
			inst.Exe, free>>20, (3*res.Asset.Size)>>20)
	}
	if !o.yes {
		if !u.interactive {
			return refuse("pas de confirmation possible sans terminal : ajouter --yes")
		}
		ok, err := u.confirm(fmt.Sprintf("Mettre à jour kura-agent %s → %s ? [o/N] ", from, to))
		if err != nil || !ok {
			return refuse("mise à jour non confirmée")
		}
	}

	staging, err := u.prepareStaging(inst)
	if err != nil {
		say("Échec : dossier de travail %s : %v", inst.Staging, err)
		return exitUpdateFailed
	}
	defer os.RemoveAll(staging)
	files, archive, err := u.fetch(ctx, inst, res, staging)
	if err != nil {
		say("Échec : %v ; rien n'a été modifié.", err)
		return exitUpdateFailed
	}
	say("• archive téléchargée, empreinte conforme à l'attestation ; nouvel exécutable : version %s", to)

	// Remplacement : le journal est écrit avant, pour qu'une coupure soit reprise.
	st := u.readStatus()
	st.Journal = &selfupdate.Journal{From: from, To: to, Mode: inst.Mode, Step: "replacing", Started: u.now()}
	u.writeStatus(st)
	if inst.Mode == selfupdate.ModeDeb {
		err = u.dpkgInstall(archive)
	} else {
		err = selfupdate.Swap(files)
	}
	if err != nil {
		st.Journal = nil
		st.Last = &selfupdate.Record{From: from, To: to, At: u.now(), Mode: inst.Mode, Result: "failed", Message: err.Error()}
		u.writeStatus(st)
		say("Échec du remplacement : %v ; version %s conservée.", err, from)
		return exitUpdateFailed
	}
	say("• exécutables remplacés (version précédente gardée pour --rollback)")

	st.Journal.Step = "finishing"
	u.writeStatus(st)
	if err := u.finishAndCheck(inst.Exe, to); err != nil {
		say("La version %s ne passe pas sa vérification : %v", to, err)
		say("Retour à la version %s…", from)
		rerr := u.restore(ctx, inst, from)
		st.Journal = nil
		if rerr != nil {
			st.Last = &selfupdate.Record{From: from, To: to, At: u.now(), Mode: inst.Mode, Result: "failed", Message: err.Error() + " ; retour arrière : " + rerr.Error()}
			u.writeStatus(st)
			say("ÉCHEC du retour arrière : %v", rerr)
			say("Réinstaller kura-agent %s à la main (kit de la console ou release GitHub), puis kura-agent install.", from)
			return exitUpdateBroken
		}
		st.Last = &selfupdate.Record{From: from, To: to, At: u.now(), Mode: inst.Mode, Result: "rolled_back", Message: err.Error()}
		u.writeStatus(st)
		say("Version %s remise ; rien d'autre n'a changé.", from)
		return exitUpdateRolledBack
	}
	st.Journal, st.Available = nil, nil
	st.Last = &selfupdate.Record{From: from, To: to, At: u.now(), Mode: inst.Mode, Result: "ok"}
	u.writeStatus(st)
	if inst.Mode == selfupdate.ModeFiles {
		selfupdate.CleanBackups(inst.Files)
	}
	say("kura-agent mis à jour : %s → %s. Configuration, secrets et planification inchangés.", from, to)
	if len(inst.Files) > 1 {
		say("L'icône passera à la nouvelle version à la prochaine ouverture de session.")
	}
	return exitUpdateOK
}

// finishAndCheck : la nouvelle version annonce la bonne version puis termine la
// mise à jour (relecture de la configuration installée).
func (u *updater) finishAndCheck(exe, want string) error {
	got, err := u.exeVersion(exe)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s annonce la version %s, %s attendue", exe, got, want)
	}
	return u.finish(exe)
}

// prepareStaging recrée le dossier de travail, réservé aux administrateurs : sous
// Linux et macOS en 0700 (root) ; sous Windows dans C:\Program Files\Kura, dont il
// hérite les droits (administrateurs et SYSTEM seuls en écriture).
func (u *updater) prepareStaging(inst installation) (string, error) {
	if err := os.RemoveAll(inst.Staging); err != nil {
		return "", err
	}
	if err := os.Mkdir(inst.Staging, 0o700); err != nil {
		return "", err
	}
	st, err := os.Lstat(inst.Staging)
	if err != nil || !st.IsDir() {
		return "", errors.New("dossier de travail inattendu")
	}
	return inst.Staging, nil
}

// fetch télécharge l'archive vérifiée, en extrait les exécutables et contrôle la
// version du nouvel exécutable. Renvoie les fichiers à remplacer et l'archive.
func (u *updater) fetch(ctx context.Context, inst installation, res selfupdate.Result, staging string) ([]selfupdate.File, string, error) {
	archive := filepath.Join(staging, res.Asset.Name)
	if err := u.download(ctx, res.Asset, archive); err != nil {
		return nil, "", err
	}
	v := res.Latest.Version
	newDir := filepath.Join(staging, "new")
	if err := os.Mkdir(newDir, 0o700); err != nil {
		return nil, "", err
	}
	var files []selfupdate.File
	var newExe string
	switch u.goos {
	case "windows":
		names := []string{"kura-agent.exe", "kura-agentw.exe"} // archive Windows, quel que soit le système des tests
		if err := selfupdate.ExtractZip(archive, "kura-agent_"+v+"_windows_amd64", newDir, names...); err != nil {
			return nil, "", err
		}
		for _, t := range inst.Files {
			files = append(files, selfupdate.File{Target: t, New: filepath.Join(newDir, filepath.Base(t))})
		}
		newExe = filepath.Join(newDir, "kura-agent.exe")
	case "darwin":
		if err := selfupdate.ExtractTarGz(archive, "kura-agent_"+v+"_darwin_"+u.goarch, newDir, "kura-agent"); err != nil {
			return nil, "", err
		}
		newExe = filepath.Join(newDir, "kura-agent")
		files = []selfupdate.File{{Target: inst.Exe, New: newExe}}
	case "linux":
		exe, err := u.debExtract(archive, newDir)
		if err != nil {
			return nil, "", err
		}
		newExe = exe
		files = []selfupdate.File{{Target: inst.Exe, New: newExe}}
	default:
		return nil, "", fmt.Errorf("système %s non pris en charge", u.goos)
	}
	got, err := u.exeVersion(newExe)
	if err != nil {
		return nil, "", err
	}
	if got != v {
		return nil, "", fmt.Errorf("le nouvel exécutable annonce la version %s, %s attendue", got, v)
	}
	return files, archive, nil
}

// restore remet la version from : renommage inverse, ou paquet de cette version
// (téléchargé et vérifié comme une mise à jour).
func (u *updater) restore(ctx context.Context, inst installation, from string) error {
	if inst.Mode == selfupdate.ModeFiles {
		if err := selfupdate.Restore(inst.Files); err != nil {
			return err
		}
	} else {
		res, err := u.check(ctx, selfupdate.Options{Current: from, Tag: "v" + from, GOOS: u.goos, GOARCH: u.goarch})
		if err != nil {
			return fmt.Errorf("paquet %s : %w", from, err)
		}
		staging, err := u.prepareStaging(inst)
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		if _, archive, err := u.fetch(ctx, inst, res, staging); err != nil {
			return err
		} else if err := u.dpkgInstall(archive); err != nil {
			return err
		}
	}
	got, err := u.exeVersion(inst.Exe)
	if err != nil {
		return err
	}
	if got != from {
		return fmt.Errorf("%s annonce %s après le retour arrière, %s attendue", inst.Exe, got, from)
	}
	return nil
}

// rollback : --rollback, retour à la version d'avant la dernière mise à jour réussie.
func (u *updater) rollback(ctx context.Context, inst installation, current string, out io.Writer) int {
	st := u.readStatus()
	last := st.Last
	if last == nil || last.Result != "ok" || last.To != current {
		fmt.Fprintf(out, "Refusé : aucune mise à jour réussie vers la version installée %s à défaire.\n", current)
		return exitUpdateRefused
	}
	fmt.Fprintf(out, "Retour de kura-agent %s à %s…\n", current, last.From)
	if err := u.restore(ctx, inst, last.From); err != nil {
		fmt.Fprintf(out, "Échec du retour arrière : %v\n", err)
		return exitUpdateFailed
	}
	st.Last = &selfupdate.Record{From: current, To: last.From, At: u.now(), Mode: inst.Mode, Result: "rollback"}
	u.writeStatus(st)
	fmt.Fprintf(out, "kura-agent %s remis. Configuration, secrets et planification inchangés.\n", last.From)
	return exitUpdateOK
}

// recover termine une mise à jour interrompue (coupure de courant, arrêt forcé) :
// version précédente remise si elle est encore à côté, journal effacé.
func (u *updater) recover(inst installation, out io.Writer) {
	st := u.readStatus()
	j := st.Journal
	if j == nil {
		return
	}
	fmt.Fprintf(out, "Mise à jour %s → %s interrompue (%s) : ", j.From, j.To, j.Step)
	msg := "journal effacé"
	if j.Mode == selfupdate.ModeFiles && inst.Mode == selfupdate.ModeFiles {
		if got, err := u.exeVersion(inst.Exe); err == nil && got == j.From {
			msg = "version " + j.From + " toujours en place"
		} else if err := selfupdate.Restore(inst.Files); err == nil {
			msg = "version " + j.From + " remise"
		} else {
			msg = "version précédente non remise (" + err.Error() + ")"
		}
	}
	fmt.Fprintln(out, msg+".")
	st.Journal = nil
	st.Last = &selfupdate.Record{From: j.From, To: j.To, At: u.now(), Mode: j.Mode, Result: "failed", Message: "interrompue ; " + msg}
	u.writeStatus(st)
}

// finishUpdate : self-update --finish, lancé par la nouvelle version juste après le
// remplacement. Elle relit la configuration installée avec ses propres règles : un
// format qu'elle refuse fait revenir à l'ancienne version. (Aucun changement du format
// de la planification depuis la 0.8 : rien à régénérer.)
func finishUpdate(stdout, stderr io.Writer, cfgPath string) int {
	cfg, err := config.Load(cfgPath, config.LoadOptions{})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintln(stdout, "aucune configuration installée : rien à relire")
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "configuration installée refusée par kura-agent %s : %v\n", version, err)
		return 1
	}
	fmt.Fprintf(stdout, "kura-agent %s : configuration relue (%d job(s))\n", version, len(cfg.Jobs))
	return 0
}

// updateInProgress : une mise à jour a écrit son journal (elle tient alors le verrou).
func updateInProgress() bool {
	st, err := selfupdate.ReadStatus(updateStatusFile())
	return err == nil && st.Journal != nil
}

// recoverInterruptedUpdate : run reprend une mise à jour interrompue s'il en a les
// droits (SYSTEM sous Windows, root sous macOS) ; sous Linux, le service tourne sous
// kura-agent et laisse la reprise à sudo kura-agent self-update.
func recoverInterruptedUpdate() {
	if !updateInProgress() || setup.CheckAdmin() != nil {
		return
	}
	inst, err := locateInstallation()
	if err != nil {
		return
	}
	realUpdater(nil, os.Stderr).recover(inst, os.Stderr)
}

// scheduledUpdateCheck : après une exécution, au plus une fois par 24 h, cherche une
// version plus récente (sans rien télécharger) et la note dans update.json pour
// kura-agent status et l'icône. Un échec ne change pas le résultat de la sauvegarde.
func scheduledUpdateCheck(cfg *config.Config) {
	path := updateStatusFile()
	st, err := selfupdate.ReadStatus(path)
	if err != nil || !st.CheckDue(time.Now()) || st.Journal != nil {
		return
	}
	hc, err := selfupdate.NewHTTPClient(cfg.Update.Proxy)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := &selfupdate.Client{HTTP: hc, UserAgent: "kura-agent/" + version}
	res, err := c.Check(ctx, selfupdate.Options{Current: version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH})
	now := time.Now()
	st.CheckedAt, st.CheckErr = &now, ""
	switch {
	case errors.Is(err, selfupdate.ErrDevVersion):
		st.Available = nil
	case err != nil:
		st.CheckErr = err.Error()
	case res.Newer:
		st.Available = &selfupdate.Available{Version: res.Latest.Version, Published: res.Latest.Published, NotesURL: res.Latest.NotesURL}
	default:
		st.Available = nil
	}
	_ = state.WriteAtomic(path, st)
}

// printUpdateStatus : version en place, version disponible, dernière mise à jour.
func printUpdateStatus(w io.Writer) {
	st, err := selfupdate.ReadStatus(updateStatusFile())
	if err != nil {
		return
	}
	line := "Agent             : " + version
	if a := st.Available; a != nil {
		if cmp, err := selfupdate.Compare(a.Version, version); err == nil && cmp > 0 {
			line += fmt.Sprintf(" ; version %s disponible (kura-agent self-update)", a.Version)
		}
	}
	fmt.Fprintln(w, line)
	if st.Journal != nil {
		fmt.Fprintf(w, "Mise à jour       : %s → %s interrompue ; relancer kura-agent self-update\n", st.Journal.From, st.Journal.To)
	}
	if l := st.Last; l != nil {
		label := map[string]string{"ok": "réussie", "rolled_back": "défaite (nouvelle version en échec)", "failed": "en échec", "rollback": "retour arrière"}[l.Result]
		fmt.Fprintf(w, "Dernière mise à jour : %s → %s le %s, %s\n", l.From, l.To, l.At.Local().Format("02/01/2006 15:04"), label)
	}
	if st.CheckErr != "" && st.CheckedAt != nil {
		fmt.Fprintf(w, "Recherche de version : échec le %s (%s)\n", st.CheckedAt.Local().Format("02/01/2006 15:04"), st.CheckErr)
	}
}
