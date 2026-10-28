package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/report"
	"github.com/E-C-G/kura-agent/internal/restore"
	"github.com/E-C-G/kura-agent/internal/selfupdate"
	"github.com/E-C-G/kura-agent/internal/setup"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Codes de sortie de restore.
const (
	exitRestoreOK      = 0
	exitRestorePartial = 1 // lot 2 : des fichiers en échec
	exitRestoreConfig  = 3 // configuration illisible, chemin hors des sources
	exitRestoreRefused = 4 // droits, options, destination
	exitRestoreBucket  = 5 // bucket injoignable ou clé refusée : rien n'a été écrit
)

// retention : fenêtre du cycle de vie du modèle de bucket (versions anciennes).
const retention = 90 * 24 * time.Hour

// restoreDeps : ce que restore demande au système, remplaçable dans les tests.
type restoreDeps struct {
	checkAdmin func() error
	loadConfig func(path string) (*config.Config, error)
	bucket     func(cfg *config.Config) (restoreBucket, error)
	now        func() time.Time
	windows    bool
	foldCase   bool // système de fichiers insensible à la casse (Windows, macOS)

	// lot 2 : restauration vers --to
	interactive bool
	confirm     func(prompt string) (bool, error)
	freeSpace   func(dir string) (uint64, error)
	owner       func(dir string) (restore.Owner, error)
	logsDir     string
	statusFile  string
	account     func() string
	report      func(ctx context.Context, cfg *config.Config, info report.RestoreInfo) error
	workers     int
	version     string

	// lot 3 : restauration en place
	lock         func() (func(), error) // verrou de la sauvegarde (run.lock)
	inPlaceOwner func() restore.Owner
}

// restoreBucket : lecture seule, lister et lire des versions.
type restoreBucket interface {
	restore.Lister
	restore.Getter
}

func realRestoreDeps() restoreDeps {
	return restoreDeps{
		checkAdmin: setup.CheckAdmin,
		loadConfig: func(p string) (*config.Config, error) { return config.Load(p, config.LoadOptions{}) },
		bucket: func(cfg *config.Config) (restoreBucket, error) {
			if cfg.Profile.AWSCredentials == "" {
				return nil, errors.New("profile.aws_credentials absent : la restauration lit la clé du poste dans le coffre (credential:<nom>)")
			}
			v, err := vault().Resolve(cfg.Profile.AWSCredentials)
			if err != nil {
				return nil, fmt.Errorf("profile.aws_credentials : %w", err)
			}
			k, err := creds.ParseAWSKeys(v)
			if err != nil {
				return nil, fmt.Errorf("profile.aws_credentials : %w", err)
			}
			return restore.NewBucket(cfg.Profile.Endpoint, cfg.Profile.Region, cfg.Profile.Bucket, k.AccessKeyID, k.SecretAccessKey, nil), nil
		},
		now:         time.Now,
		windows:     runtime.GOOS == "windows",
		foldCase:    runtime.GOOS == "windows" || runtime.GOOS == "darwin",
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
		freeSpace:  selfupdate.FreeSpace,
		owner:      destOwner,
		logsDir:    paths.LogsDir(),
		statusFile: paths.RestoreFile(),
		account:    currentAccount,
		report: func(ctx context.Context, cfg *config.Config, info report.RestoreInfo) error {
			return report.Restore(ctx, cfg.Notify.Report, vault(), reportOptions(cfg), info)
		},
		workers: 8,
		version: version,
		lock: func() (func(), error) {
			r, err := lock.Acquire()
			return func() { r() }, err
		},
		inPlaceOwner: inPlaceOwner,
	}
}

func cmdRestore(args []string) int {
	if len(args) > 0 && args[0] == "--serve" {
		// Tâche \Kura\Restore : sert les fenêtres de restauration de l'icône.
		fs := flag.NewFlagSet("restore --serve", flag.ContinueOnError)
		cfgPath := fs.String("config", paths.ConfigFile(), "fichier de configuration installé")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		sd, closeLog := realServeDeps()
		defer closeLog()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return serveRestore(ctx, sd, *cfgPath)
	}
	return runRestore(args, os.Stdout, os.Stderr, realRestoreDeps())
}

// splitPositional sépare le chemin (premier argument qui n'est pas une option) des
// options, où qu'il soit : « restore CHEMIN --list » comme « restore --list CHEMIN ».
func splitPositional(args []string, withValue map[string]bool) (pos []string, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && withValue[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return pos, flags
}

func runRestore(args []string, stdout, stderr io.Writer, d restoreDeps) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	versions := fs.Bool("versions", false, "historique d'un fichier : ses versions conservées, sans rien écrire")
	list := fs.Bool("list", false, "ce qui serait restauré (fichiers, tailles, total), sans rien écrire")
	atFlag := fs.String("at", "", "date de l'état : AAAA-MM-JJ (fin de journée) ou \"AAAA-MM-JJ HH:MM\", heure locale")
	all := fs.Bool("all", false, "tous les jobs de la configuration")
	to := fs.String("to", "", "dossier où restaurer, absent ou vide, hors des sources des jobs")
	inPlace := fs.Bool("in-place", false, "remet à leur place les fichiers absents ; ceux qui existent sont laissés")
	replace := fs.Bool("replace", false, "avec --in-place : remplace aussi ceux qui diffèrent, l'actuel gardé à côté")
	yes := fs.Bool("yes", false, "sans confirmation (obligatoire hors d'un terminal)")
	asJSON := fs.Bool("json", false, "résultat en JSON")
	cfgPath := fs.String("config", paths.ConfigFile(), "fichier de configuration installé")
	pos, flags := splitPositional(args, map[string]bool{"at": true, "to": true, "config": true})
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	refuse := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "Refusé : "+format+"\n", a...)
		return exitRestoreRefused
	}
	switch {
	case len(pos) > 1:
		return refuse("un seul chemin à la fois (entre guillemets s'il contient des espaces)")
	case *all && len(pos) == 1:
		return refuse("--all ou un chemin, pas les deux")
	case !*all && len(pos) == 0:
		return refuse("chemin à restaurer manquant (ou --all)")
	case *versions && *list:
		return refuse("--versions ou --list, pas les deux")
	case *versions && (*all || *atFlag != ""):
		return refuse("--versions vaut pour un fichier, sans --all ni --at")
	case *versions && (*to != "" || *inPlace):
		return refuse("--versions n'écrit rien : sans --to ni --in-place")
	case *inPlace && *to != "":
		return refuse("--to ou --in-place, pas les deux")
	case *replace && !*inPlace:
		return refuse("--replace s'emploie avec --in-place")
	case !*versions && !*list && *to == "" && !*inPlace:
		return refuse("destination manquante : --to DOSSIER, ou --in-place (ou --list pour voir ce qui serait restauré, --versions pour l'historique d'un fichier)")
	}
	if err := d.checkAdmin(); err != nil {
		return refuse("%v", err)
	}
	now := d.now()
	var at time.Time
	if *atFlag != "" {
		t, err := parseRestoreDate(*atFlag, now.Location())
		if err != nil {
			return refuse("%v", err)
		}
		if t.After(now) {
			t = time.Time{} // date à venir : état de la dernière sauvegarde
		}
		at = t
	}
	cfg, err := d.loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRestoreConfig
	}
	var targets []restore.Target
	if *all {
		for _, j := range cfg.Jobs {
			if j.Unavailable != "" {
				fmt.Fprintf(stderr, "Job %s ignoré : %s\n", j.ID, j.Unavailable)
				continue
			}
			t, err := restore.Resolve(cfg, j.Source, d.windows)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return exitRestoreConfig
			}
			targets = append(targets, t)
		}
	} else {
		t, err := restore.Resolve(cfg, absLocal(pos[0], d.windows), d.windows)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRestoreConfig
		}
		targets = append(targets, t)
	}
	b, err := d.bucket(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRestoreConfig
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	if *versions {
		return restoreVersions(listCtx, stdout, stderr, b, targets[0], d.windows, *asJSON)
	}
	if !at.IsZero() && now.Sub(at) > retention {
		fmt.Fprintf(stderr, "Attention : au-delà de 90 jours, les versions remplacées ont été effacées par le cycle de vie du bucket : l'état du %s peut être incomplet.\n", at.Format("02/01/2006"))
	}
	// En place : le verrou de la sauvegarde est pris avant l'inventaire, pour que la
	// sauvegarde planifiée ne tourne pas pendant la restauration (« déjà en cours »).
	if *inPlace && !*list {
		release, err := d.lock()
		if err != nil {
			if errors.Is(err, lock.ErrBusy) {
				return refuse("une sauvegarde est en cours : relancer la restauration quand elle sera finie")
			}
			return refuse("verrou de la sauvegarde : %v", err)
		}
		defer release()
	}
	// Destination vérifiée avant toute lecture du bucket.
	var dest *destCheck
	if *to != "" && !*list {
		var code int
		if dest, code = checkDest(stderr, d, cfg, *to, targets, at); dest == nil {
			return code
		}
	}
	var reports []listReport
	for _, t := range targets {
		vs, err := listTarget(listCtx, b, t, d.windows)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitRestoreBucket
		}
		reports = append(reports, listReport{target: t, files: restore.StateAt(vs, t, at, d.windows)})
	}
	if dest != nil {
		return restoreTo(ctx, stdout, stderr, d, cfg, b, dest, reports, at, *all, *yes, *asJSON, args)
	}
	if *inPlace {
		return restoreInPlace(ctx, stdout, stderr, d, cfg, b, reports, at, *replace, *list, *yes, *asJSON, args)
	}
	if *asJSON {
		return printListJSON(stdout, reports, at, d.windows)
	}
	for i, r := range reports {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		printList(stdout, r, at, d.windows)
	}
	return exitRestoreOK
}

// dispPath : chemin tel que le système l'écrit (\ sous Windows).
func dispPath(p string, windows bool) string {
	if windows {
		return strings.ReplaceAll(p, "/", `\`)
	}
	return p
}

// absLocal rend absolu un chemin relatif au dossier courant. Un chemin Windows déjà
// absolu (C:\..., C:/..., \\serveur\partage) est gardé tel quel, y compris quand les
// tests simulent Windows sur un autre système.
func absLocal(p string, windows bool) string {
	if windows {
		if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') || strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
			return p
		}
	} else if filepath.IsAbs(p) {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// listTarget liste les versions de la cible. Sous Windows, la casse tapée peut
// différer de celle des clés (le disque l'ignore, le bucket non) : sans résultat, le
// job entier est relu et comparé sans tenir compte de la casse.
func listTarget(ctx context.Context, b restore.Lister, t restore.Target, foldCase bool) ([]restore.Version, error) {
	vs, err := b.ListVersions(ctx, t.Prefix())
	if err != nil || len(vs) > 0 || !foldCase || t.Root {
		return vs, err
	}
	root := strings.Trim(t.Job.Destination, "/")
	if root != "" {
		root += "/"
	}
	return b.ListVersions(ctx, root)
}

type listReport struct {
	target restore.Target
	files  []restore.File
}

func restoreVersions(ctx context.Context, stdout, stderr io.Writer, b restore.Lister, t restore.Target, foldCase, asJSON bool) int {
	if t.Root {
		fmt.Fprintf(stderr, "Refusé : %s est la source entière du job %s ; --versions vaut pour un fichier, --list pour un dossier.\n", dispPath(t.Local, foldCase), t.Job.ID)
		return exitRestoreRefused
	}
	vs, err := listTarget(ctx, b, t, foldCase)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRestoreBucket
	}
	h := restore.History(vs, t.Key, foldCase)
	if len(h) == 0 && hasBelow(vs, t, foldCase) {
		fmt.Fprintf(stderr, "Refusé : %s est un dossier ; --versions vaut pour un fichier, --list pour un dossier.\n", dispPath(t.Local, foldCase))
		return exitRestoreRefused
	}
	if asJSON {
		type jv struct {
			Date      time.Time `json:"date"`
			Size      int64     `json:"size"`
			Deleted   bool      `json:"deleted,omitempty"`
			Current   bool      `json:"current,omitempty"`
			VersionID string    `json:"version_id"`
		}
		out := struct {
			Path     string `json:"path"`
			Job      string `json:"job"`
			Key      string `json:"key"`
			Versions []jv   `json:"versions"`
		}{Path: dispPath(t.Local, foldCase), Job: t.Job.ID, Key: t.Key, Versions: []jv{}}
		for _, v := range h {
			out.Versions = append(out.Versions, jv{Date: v.LastModified, Size: v.Size, Deleted: v.Deleted, Current: v.Latest, VersionID: v.VersionID})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return exitRestoreOK
	}
	fmt.Fprintf(stdout, "Historique de %s (job %s, clé %s)\n", dispPath(t.Local, foldCase), t.Job.ID, t.Key)
	if len(h) == 0 {
		fmt.Fprintln(stdout, "Aucune version : jamais sauvegardé, exclu par le job, ou effacé depuis plus de 90 jours.")
		return exitRestoreOK
	}
	for _, v := range h {
		state := ""
		switch {
		case v.Deleted:
			state = "supprimé sur le poste"
		case v.Latest:
			state = "version actuelle"
		}
		size := "—"
		if !v.Deleted {
			size = humanSize(v.Size)
		}
		fmt.Fprintf(stdout, "  %s  %10s  %s\n", v.LastModified.Local().Format("02/01/2006 15:04"), size, state)
	}
	n := 0
	for _, v := range h {
		if !v.Deleted {
			n++
		}
	}
	fmt.Fprintf(stdout, "%d version(s) conservée(s) ; une version remplacée ou supprimée est gardée 90 jours.\n", n)
	return exitRestoreOK
}

func hasBelow(vs []restore.Version, t restore.Target, foldCase bool) bool {
	for _, v := range vs {
		if t.Covers(v.Key, foldCase) && len(v.Key) > len(t.Key) {
			return true
		}
	}
	return false
}

func printList(w io.Writer, r listReport, at time.Time, windows bool) {
	when := "à la dernière sauvegarde"
	if !at.IsZero() {
		when = "au " + at.Local().Format("02/01/2006 15:04")
	}
	fmt.Fprintf(w, "État de %s %s (job %s) :\n", dispPath(r.target.Local, windows), when, r.target.Job.ID)
	if len(r.files) == 0 {
		fmt.Fprintln(w, "  aucun fichier à cette date.")
		return
	}
	problems := 0
	for _, f := range r.files {
		note := ""
		switch {
		case f.Problem != "":
			note = "  (nom refusé : " + f.Problem + ")"
			problems++
		case f.DeletedSince != nil:
			note = "  (supprimé du poste le " + f.DeletedSince.Local().Format("02/01/2006") + ")"
		case f.NewerSince:
			note = "  (modifié depuis)"
		}
		fmt.Fprintf(w, "  %-60s %10s  sauvegardé le %s%s\n", dispPath(f.Rel, windows), humanSize(f.Version.Size),
			f.Version.LastModified.Local().Format("02/01/2006 15:04"), note)
	}
	fmt.Fprintf(w, "%d fichier(s), %s.", len(r.files), humanSize(restore.Total(r.files)))
	if problems > 0 {
		fmt.Fprintf(w, " %d nom(s) refusé(s) ne seraient pas restaurés.", problems)
	}
	fmt.Fprintln(w)
}

func printListJSON(w io.Writer, reports []listReport, at time.Time, windows bool) int {
	type jf struct {
		Path         string     `json:"path"`
		Size         int64      `json:"size"`
		BackedUp     time.Time  `json:"backed_up"`
		VersionID    string     `json:"version_id"`
		DeletedSince *time.Time `json:"deleted_since,omitempty"`
		NewerSince   bool       `json:"newer_since,omitempty"`
		Problem      string     `json:"problem,omitempty"`
	}
	type jt struct {
		Path  string `json:"path"`
		Job   string `json:"job"`
		Files []jf   `json:"files"`
		Total int64  `json:"total_size"`
	}
	out := struct {
		At      *time.Time `json:"at,omitempty"`
		Targets []jt       `json:"targets"`
	}{}
	if !at.IsZero() {
		out.At = &at
	}
	for _, r := range reports {
		t := jt{Path: dispPath(r.target.Local, windows), Job: r.target.Job.ID, Files: []jf{}, Total: restore.Total(r.files)}
		for _, f := range r.files {
			t.Files = append(t.Files, jf{Path: dispPath(f.Rel, windows), Size: f.Version.Size, BackedUp: f.Version.LastModified,
				VersionID: f.Version.VersionID, DeletedSince: f.DeletedSince, NewerSince: f.NewerSince, Problem: f.Problem})
		}
		out.Targets = append(out.Targets, t)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
	return exitRestoreOK
}

// parseRestoreDate lit --at : date seule (fin de journée) ou date et heure, locales.
func parseRestoreDate(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t.Add(24*time.Hour - time.Second), nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--at %q : AAAA-MM-JJ ou \"AAAA-MM-JJ HH:MM\" attendu", s)
}

// humanSize : taille lisible, à la française (1,2 Mo).
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d o", n)
	}
	v, i := float64(n)/unit, 0
	for v >= unit && i < 3 {
		v /= unit
		i++
	}
	return strings.Replace(fmt.Sprintf("%.1f %s", v, []string{"Ko", "Mo", "Go", "To"}[i]), ".", ",", 1)
}
