package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// FieldError est une erreur rattachée à un champ de la configuration.
type FieldError struct {
	Field string
	Msg   string
}

func (e FieldError) Error() string { return e.Field + " : " + e.Msg }

// ValidationErrors regroupe toutes les erreurs trouvées, pour les corriger en une fois.
type ValidationErrors []FieldError

func (e ValidationErrors) Error() string {
	lines := make([]string, len(e))
	for i, fe := range e {
		lines[i] = "- " + fe.Error()
	}
	return fmt.Sprintf("configuration invalide (%d erreur(s)) :\n%s", len(e), strings.Join(lines, "\n"))
}

// ValidateOptions règle les vérifications liées à la machine.
type ValidateOptions struct {
	CheckSystem bool
	Env         Env
}

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9-]+$`)
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	timePattern   = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

var (
	validChecksums = set("", "CRC32", "CRC32C", "SHA1", "SHA256", "CRC64NVME")
	validSchedules = set("daily", "weekly", "hourly")
	validDays      = set("mon", "tue", "wed", "thu", "fri", "sat", "sun")
	validToasts    = set("success", "warning", "failure")
	validLevels    = set("debug", "info", "warn", "error")
)

// Validate vérifie la configuration et renvoie toutes les erreurs d'un coup.
func (c *Config) Validate(opt ValidateOptions) error {
	if opt.Env == nil {
		opt.Env = SystemEnv()
	}
	var errs ValidationErrors
	add := func(field, format string, a ...any) {
		errs = append(errs, FieldError{Field: field, Msg: fmt.Sprintf(format, a...)})
	}

	if c.Version != CurrentVersion {
		add("version", "version %d non prise en charge (attendu : %d)", c.Version, CurrentVersion)
	}

	// profile
	p := c.Profile
	if p.Bucket == "" {
		add("profile.bucket", "obligatoire")
	} else if !bucketPattern.MatchString(p.Bucket) {
		add("profile.bucket", "nom de bucket invalide %q (minuscules, chiffres, '.' et '-', 3 à 63 caractères)", p.Bucket)
	}
	if p.Endpoint == "" {
		add("profile.endpoint", "obligatoire (ex. https://s3.eu-central-1.wasabisys.com)")
	} else if u, err := url.Parse(p.Endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
		add("profile.endpoint", "URL HTTPS attendue, reçu %q", p.Endpoint)
	}
	if p.AWSCredentials != "" {
		if name, ok := strings.CutPrefix(p.AWSCredentials, CredentialPrefix); !ok || !idPattern.MatchString(name) {
			add("profile.aws_credentials", "référence attendue sous la forme credential:<nom> ; ne mettez jamais de clé en clair ici")
		}
		if p.AWSProfile != "" {
			add("profile.aws_profile", "à retirer : profile.aws_credentials fournit déjà les clés")
		}
	}
	if !validChecksums[p.ChecksumAlgorithm] {
		add("profile.checksum_algorithm", "valeur %q inconnue (CRC32, CRC32C, SHA1, SHA256, CRC64NVME)", p.ChecksumAlgorithm)
	}

	// schedule
	s := c.Schedule
	if !validSchedules[s.Type] {
		add("schedule.type", "valeur %q inconnue (daily, weekly, hourly)", s.Type)
	}
	if (s.Type == "daily" || s.Type == "weekly") && !timePattern.MatchString(s.Time) {
		add("schedule.time", "heure HH:MM attendue, reçu %q", s.Time)
	}
	for i, d := range s.Days {
		if !validDays[d] {
			add(fmt.Sprintf("schedule.days[%d]", i), "jour %q inconnu (mon, tue, wed, thu, fri, sat, sun)", d)
		}
	}
	if s.Type == "weekly" && len(s.Days) == 0 {
		add("schedule.days", "au moins un jour pour une planification weekly")
	}
	if s.MaxDuration.Duration < 0 {
		add("schedule.max_duration", "durée négative")
	}

	// pre_tasks
	preProcesses := map[string]bool{}
	for i, t := range c.PreTasks {
		f := fmt.Sprintf("pre_tasks[%d]", i)
		if t.Type != "close_process" {
			add(f+".type", "type %q inconnu (close_process)", t.Type)
		}
		if t.Process == "" {
			add(f+".process", "obligatoire (ex. outlook.exe)")
		} else {
			if preProcesses[strings.ToLower(t.Process)] {
				add(f+".process", "%q déjà déclaré dans une autre pré-tâche", t.Process)
			}
			preProcesses[strings.ToLower(t.Process)] = true
		}
		if t.NoticeSeconds < 0 {
			add(f+".notice_seconds", "valeur négative")
		}
		if t.AllowPostpone != nil && (t.AllowPostpone.Max < 0 || t.AllowPostpone.Delay.Duration <= 0) {
			add(f+".allow_postpone", "delay > 0 et max >= 0 attendus")
		}
	}

	// post_tasks
	for i, t := range c.PostTasks {
		f := fmt.Sprintf("post_tasks[%d]", i)
		if t.Type != "start_process" {
			add(f+".type", "type %q inconnu (start_process)", t.Type)
		}
		if t.Process == "" {
			add(f+".process", "obligatoire")
		}
	}

	// tools
	for _, t := range []struct{ field, path string }{
		{"tools.pg_dump", c.Tools.PgDump}, {"tools.mysqldump", c.Tools.MySQLDump}, {"tools.sqlcmd", c.Tools.SQLCmd},
		{"tools.zip", c.Tools.Zip}, {"tools.gpg", c.Tools.GPG},
	} {
		if t.path != "" && !absPath(t.path) {
			add(t.field, "chemin absolu attendu (l'outil n'est jamais cherché dans le PATH), reçu %q", t.path)
		}
	}
	if c.Tools.Zip != "" && ZipFlavor(c.Tools.Zip) == "" {
		add("tools.zip", "%q : zip (Info-ZIP) ou 7z, 7za, 7zz (7-Zip) attendu", c.Tools.Zip)
	}

	// prepare
	prepares := map[string]int{}
	for i, pr := range c.Prepare {
		f := fmt.Sprintf("prepare[%d]", i)
		switch _, dup := prepares[pr.ID]; {
		case pr.ID == "":
			add(f+".id", "obligatoire")
		case !idPattern.MatchString(pr.ID):
			add(f+".id", "%q : minuscules, chiffres et '-' uniquement", pr.ID)
		case reservedName(pr.ID):
			add(f+".id", "%q : nom réservé par Windows (périphérique)", pr.ID)
		case dup:
			add(f+".id", "%q déjà utilisé", pr.ID)
		default:
			prepares[pr.ID] = i
		}
		if pr.Timeout.Duration < 0 {
			add(f+".timeout", "durée négative")
		}
		if pr.DependsOnPreTask != "" && !preProcesses[strings.ToLower(pr.DependsOnPreTask)] {
			add(f+".depends_on_pre_task", "aucune pré-tâche ne ferme %q", pr.DependsOnPreTask)
		}
		switch pr.Type {
		case PrepareZip:
			if c.Tools.Zip == "" {
				add(f+".type", "zip demande tools.zip : chemin absolu de zip (Info-ZIP) ou de 7z (7-Zip), installé sur le poste")
			}
			switch {
			case pr.Source == "":
				add(f+".source", "obligatoire (dossier à archiver)")
			case pr.Unavailable != "":
				if opt.CheckSystem {
					add(f+".source", "%s", pr.Unavailable)
				}
			case !absPath(pr.Source):
				add(f+".source", "chemin absolu attendu, reçu %q", pr.Source)
			case opt.CheckSystem:
				if st, err := os.Stat(pr.Source); err != nil {
					add(f+".source", "%q inaccessible : %v", pr.Source, err)
				} else if !st.IsDir() {
					add(f+".source", "%q n'est pas un dossier", pr.Source)
				}
			}
			if pr.Level != LevelDeflate && pr.Level != LevelStore {
				add(f+".level", "valeur %q inconnue (deflate, store)", pr.Level)
			}
			if pr.OnLocked != OnLockedFail && pr.OnLocked != OnLockedSkip {
				add(f+".on_locked", "valeur %q inconnue (fail, skip)", pr.OnLocked)
			}
		case PrepareDBExport, PrepareGPG, PrepareAge:
			add(f+".type", "type %q pas encore disponible dans cette version de l'agent (zip seulement)", pr.Type)
		case "":
			add(f+".type", "obligatoire (zip)")
		default:
			add(f+".type", "type %q inconnu (zip, db_export, gpg, age)", pr.Type)
		}
	}
	usedPrepares := map[string]bool{}

	// jobs
	if len(c.Jobs) == 0 {
		add("jobs", "au moins un job")
	}
	ids := map[string]bool{}
	type dest struct {
		field, path string
	}
	var dests []dest
	for i, j := range c.Jobs {
		f := fmt.Sprintf("jobs[%d]", i)
		switch {
		case j.ID == "":
			add(f+".id", "obligatoire")
		case !idPattern.MatchString(j.ID):
			add(f+".id", "%q : minuscules, chiffres et '-' uniquement", j.ID)
		case reservedName(j.ID):
			add(f+".id", "%q : nom réservé par Windows (périphérique)", j.ID)
		case ids[j.ID]:
			add(f+".id", "%q déjà utilisé", j.ID)
		}
		ids[j.ID] = true

		switch {
		case j.Prepare != "":
			// Zone de préparation : remplie à chaque exécution, absente avant.
			if _, ok := prepares[j.Prepare]; !ok {
				add(f+".source", "${STAGING:%s} : aucune préparation n'a cet id", j.Prepare)
			}
			usedPrepares[j.Prepare] = true
		case j.Source == "":
			add(f+".source", "obligatoire")
		case j.Unavailable != "":
			// run passe outre (seul ce job échoue) ; validate et install le signalent.
			if opt.CheckSystem {
				add(f+".source", "%s", j.Unavailable)
			}
		case opt.CheckSystem:
			if st, err := os.Stat(j.Source); err != nil {
				add(f+".source", "%q inaccessible : %v", j.Source, err)
			} else if !st.IsDir() {
				add(f+".source", "%q n'est pas un dossier", j.Source)
			}
		}

		switch {
		case j.Destination == "":
			add(f+".destination", "obligatoire (préfixe dans le bucket)")
		case strings.HasPrefix(j.Destination, "/") || strings.HasSuffix(j.Destination, "/"):
			add(f+".destination", "%q ne doit ni commencer ni finir par '/'", j.Destination)
		default:
			for _, d := range dests {
				if nested(d.path, j.Destination) {
					add(f+".destination", "%q recouvre la destination de %s (%q)", j.Destination, d.field, d.path)
				}
			}
			dests = append(dests, dest{field: f, path: j.Destination})
		}

		if g := j.Guard; g != nil {
			if g.MinSourceFiles < 0 || g.MaxDeletes < 0 {
				add(f+".guard", "valeurs négatives interdites")
			}
			if g.MaxDeleteRatio < 0 || g.MaxDeleteRatio > 1 {
				add(f+".guard.max_delete_ratio", "entre 0 et 1 attendu, reçu %v", g.MaxDeleteRatio)
			}
		}
		if j.DependsOnPreTask != "" && !preProcesses[strings.ToLower(j.DependsOnPreTask)] {
			add(f+".depends_on_pre_task", "aucune pré-tâche ne ferme %q", j.DependsOnPreTask)
		}
	}

	for i, pr := range c.Prepare {
		if pr.ID != "" && !usedPrepares[pr.ID] {
			add(fmt.Sprintf("prepare[%d].id", i), "%q : aucun job ne l'envoie (source: ${STAGING:%s})", pr.ID, pr.ID)
		}
	}

	// tray
	checkAccounts := func(field string, list []string) {
		for i, a := range list {
			switch {
			case strings.TrimSpace(a) == "":
				add(fmt.Sprintf("%s[%d]", field, i), "nom de compte vide")
			case opt.CheckSystem && !opt.Env.AccountExists(a):
				add(fmt.Sprintf("%s[%d]", field, i), "le compte %q n'existe pas sur cette machine", a)
			}
		}
	}
	checkAccounts("tray.users", c.Tray.Users)
	checkAccounts("tray.allow_run_now", c.Tray.AllowRunNow)

	// update
	if px := c.Update.Proxy; px != "" {
		u, err := url.Parse(px)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "":
			add("update.proxy", "URL de proxy attendue, ex. http://proxy.example.com:3128 ; reçu %q", px)
		case u.User != nil:
			add("update.proxy", "identifiants interdits dans l'URL du proxy (fichier lisible) : proxy sans authentification ou filtré par adresse")
		}
	}

	// notify
	for i, t := range c.Notify.ToastOn {
		if !validToasts[t] {
			add(fmt.Sprintf("notify.toast_on[%d]", i), "valeur %q inconnue (success, warning, failure)", t)
		}
	}
	if w := c.Notify.Webhook; w != nil && w.URL != "" && c.Notify.Report != nil &&
		(c.Notify.Report.URL != w.URL || c.Notify.Report.Type != ReportWebhook) {
		add("notify.webhook", "ancienne forme de notify.report : ne gardez que notify.report")
	}
	if r := c.Notify.Report; r != nil {
		switch r.Type {
		case ReportHealthchecks, ReportWebhook:
		case "":
			add("notify.report.type", "obligatoire : healthchecks ou webhook")
		default:
			add("notify.report.type", "valeur %q inconnue (healthchecks, webhook)", r.Type)
		}
		switch {
		case r.URL == "":
			add("notify.report.url", "obligatoire")
		case strings.HasPrefix(r.URL, CredentialPrefix):
			if name := strings.TrimPrefix(r.URL, CredentialPrefix); !idPattern.MatchString(name) {
				add("notify.report.url", "nom de secret %q invalide (minuscules, chiffres et '-')", name)
			}
		default:
			if u, err := url.Parse(r.URL); err != nil || u.Scheme != "https" || u.Host == "" {
				add("notify.report.url", "URL HTTPS ou credential:<nom> attendue, reçu %q", r.URL)
			}
		}
		if r.Token != "" {
			if r.Type == ReportHealthchecks {
				add("notify.report.token", "inutile avec healthchecks : l'URL de ping identifie le check")
			} else if name, ok := strings.CutPrefix(r.Token, CredentialPrefix); !ok || !idPattern.MatchString(name) {
				add("notify.report.token", "référence attendue sous la forme credential:<nom> ; ne mettez jamais le jeton en clair ici")
			}
		}
		if r.HMAC && (r.Type != ReportWebhook || r.Token == "") {
			add("notify.report.hmac", "la signature HMAC demande type: webhook et un token")
		}
	}

	// logging
	if !validLevels[c.Logging.Level] {
		add("logging.level", "valeur %q inconnue (debug, info, warn, error)", c.Logging.Level)
	}
	if c.Logging.RetentionDays < 1 {
		add("logging.retention_days", "au moins 1 jour")
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// absPath : chemin absolu Windows (C:\… ou C:/…) ou Unix (/…), quel que soit le système
// qui vérifie (kura-ctl prépare des configurations pour d'autres postes).
func absPath(p string) bool {
	return strings.HasPrefix(p, "/") || drivePath.MatchString(p)
}

var drivePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// reservedName : nom de périphérique Windows (con, nul, com1…), inutilisable comme nom de
// fichier ou de dossier (journal du job, dossier de la préparation).
func reservedName(id string) bool {
	switch id {
	case "con", "prn", "aux", "nul":
		return true
	}
	if len(id) == 4 && (strings.HasPrefix(id, "com") || strings.HasPrefix(id, "lpt")) && id[3] >= '0' && id[3] <= '9' {
		return true
	}
	return false
}

// Saveurs d'outil zip.
const (
	ZipInfoZIP = "info-zip"
	Zip7Zip    = "7-zip"
)

// ZipFlavor reconnaît l'outil de tools.zip à son nom : Info-ZIP (zip) ou 7-Zip (7z, 7za,
// 7zz) ; "" sinon.
func ZipFlavor(path string) string {
	base := strings.ToLower(path[strings.LastIndexAny(path, `/\`)+1:])
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "zip":
		return ZipInfoZIP
	case "7z", "7za", "7zz":
		return Zip7Zip
	}
	return ""
}

// nested indique si deux préfixes S3 se recouvrent (identiques ou l'un dans l'autre).
func nested(a, b string) bool {
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}
