// Package config définit le format de configuration de kura-agent
// (config.yaml), son chargement, l'expansion des variables et sa validation.
//
// Le paquet est public (pkg/) car kura-ctl l'importe pour générer les paquets
// d'installation : une seule définition du format fait foi des deux côtés.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// CurrentVersion est la seule version du format acceptée par cette build.
const CurrentVersion = 1

// Config est le contenu de config.yaml.
type Config struct {
	Version   int        `yaml:"version"`
	Profile   Profile    `yaml:"profile"`
	Schedule  Schedule   `yaml:"schedule"`
	PreTasks  []PreTask  `yaml:"pre_tasks"`
	PostTasks []PostTask `yaml:"post_tasks"`
	Jobs      []Job      `yaml:"jobs"`
	Tray      Tray       `yaml:"tray"`
	Notify    Notify     `yaml:"notify"`
	Logging   Logging    `yaml:"logging"`
}

// Profile décrit la destination Wasabi et l'outil de copie.
type Profile struct {
	Name       string `yaml:"name"`
	AWSCLI     string `yaml:"aws_cli"`     // chemin de aws.exe ; vide = recherche dans le PATH
	AWSProfile string `yaml:"aws_profile"` // profil de ~/.aws/credentials ; jamais de clé ici
	// AWSCredentials désigne les clés d'accès rangées dans le coffre de l'agent
	// (credential:<nom>, saisies par `kura-agent credentials set-aws <nom>`). C'est la
	// forme à utiliser avec le compte svc-kura, qui n'a pas de ~/.aws. Exclut aws_profile.
	AWSCredentials    string `yaml:"aws_credentials"`
	Endpoint          string `yaml:"endpoint"`
	Region            string `yaml:"region"`
	Bucket            string `yaml:"bucket"`
	ChecksumAlgorithm string `yaml:"checksum_algorithm"`
}

// Schedule décrit le déclenchement de la tâche planifiée.
type Schedule struct {
	Type        string   `yaml:"type"` // daily | weekly | hourly
	Time        string   `yaml:"time"` // HH:MM, pour daily et weekly
	Days        []string `yaml:"days"` // mon..sun
	RunIfMissed bool     `yaml:"run_if_missed"`
	MaxDuration Duration `yaml:"max_duration"`
}

// PreTask est une action exécutée avant les jobs (V1 : fermer un processus).
type PreTask struct {
	Type            string    `yaml:"type"` // close_process
	Process         string    `yaml:"process"`
	NoticeSeconds   int       `yaml:"notice_seconds"`
	GracefulTimeout Duration  `yaml:"graceful_timeout"`
	Force           bool      `yaml:"force"`
	AllowPostpone   *Postpone `yaml:"allow_postpone"`
}

// Postpone autorise l'utilisateur à reporter la fermeture.
type Postpone struct {
	Delay Duration `yaml:"delay"`
	Max   int      `yaml:"max"`
}

// PostTask est une action exécutée après les jobs (V1 : relancer un processus).
type PostTask struct {
	Type             string `yaml:"type"` // start_process
	Process          string `yaml:"process"`
	OnlyIfWasRunning bool   `yaml:"only_if_was_running"`
}

// Job est une synchronisation source locale -> préfixe du bucket.
type Job struct {
	ID               string   `yaml:"id"`
	Name             string   `yaml:"name"`
	Source           string   `yaml:"source"`
	Destination      string   `yaml:"destination"`
	Delete           bool     `yaml:"delete"`
	Exclude          []string `yaml:"exclude"`
	Guard            *Guard   `yaml:"guard"`
	DependsOnPreTask string   `yaml:"depends_on_pre_task"`

	// Unavailable : la source ou la destination ne se résout pas sur cette machine
	// (ex. ${PROFILE:bob} d'un compte supprimé). Le job finit en erreur avec ce
	// message ; les autres jobs s'exécutent. Rempli par Expand, jamais lu du YAML.
	Unavailable string `yaml:"-"`
}

// Unavailable renvoie les jobs inutilisables sur cette machine (voir Job.Unavailable).
func (c *Config) Unavailable() []Job {
	var out []Job
	for _, j := range c.Jobs {
		if j.Unavailable != "" {
			out = append(out, j)
		}
	}
	return out
}

// Guard est le garde-fou contre les suppressions massives.
type Guard struct {
	MinSourceFiles int     `yaml:"min_source_files"`
	MaxDeletes     int     `yaml:"max_deletes"`      // 0 = pas de plafond absolu
	MaxDeleteRatio float64 `yaml:"max_delete_ratio"` // 0 = pas de plafond relatif
}

// Tray liste les comptes qui reçoivent l'icône et ceux qui peuvent lancer la tâche.
type Tray struct {
	Users       []string `yaml:"users"`
	AllowRunNow []string `yaml:"allow_run_now"`
	// LateAfter : sans sauvegarde réussie depuis cette durée, l'icône passe en
	// « Retard » et prévient une fois par jour. Défaut 48h.
	LateAfter Duration `yaml:"late_after"`
}

// MatchAccount indique si le compte de configuration name (« alice » ou
// « DOMAINE\alice ») désigne l'utilisateur user (« MACHINE\alice », « alice »).
func MatchAccount(name, user string) bool {
	if strings.Contains(name, `\`) {
		return strings.EqualFold(name, user)
	}
	if i := strings.LastIndex(user, `\`); i >= 0 {
		user = user[i+1:]
	}
	return strings.EqualFold(name, user)
}

// Listed indique si user figure dans la liste de comptes names.
func Listed(names []string, user string) bool {
	for _, n := range names {
		if MatchAccount(n, user) {
			return true
		}
	}
	return false
}

// Notify règle les notifications locales et l'envoi du bilan de chaque exécution.
type Notify struct {
	ToastOn []string `yaml:"toast_on"`
	Report  *Report  `yaml:"report"`
	// Webhook est l'ancienne forme (v0.1.0) de report, toujours lue :
	// une URL non vide équivaut à report: {type: webhook, …}.
	Webhook *Webhook `yaml:"webhook"`
}

// Types de destination du bilan.
const (
	ReportHealthchecks = "healthchecks" // healthchecks.io ou instance auto-hébergée
	ReportWebhook      = "webhook"      // broker générique (Make, n8n…) : POST JSON
)

// Report est la destination du bilan de chaque exécution.
//
// healthchecks : url = URL de ping du check (ex. https://hc-ping.com/<uuid>) ;
// l'agent appelle <url>/start au début, <url> en cas de succès, <url>/fail sinon.
//
// webhook : url reçoit en POST le bilan JSON ; token (facultatif) part dans
// l'en-tête X-Kura-Token, et signe le corps (X-Kura-Signature) si hmac est vrai.
//
// url et token acceptent une référence "credential:<nom>" vers le coffre de l'agent
// (kura-agent credentials set <nom>), pour ne pas laisser d'URL secrète dans ce fichier.
type Report struct {
	Type  string `yaml:"type"`
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
	HMAC  bool   `yaml:"hmac"`
}

// Webhook est l'ancienne forme de la destination du bilan.
type Webhook struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
	HMAC  bool   `yaml:"hmac"`
}

// CredentialPrefix préfixe une référence au coffre de secrets de l'agent.
const CredentialPrefix = "credential:"

// CredentialRef est une référence credential:<nom> trouvée dans la configuration.
type CredentialRef struct {
	Name  string // nom dans le coffre
	Field string // champ qui la porte, ex. notify.report.token
	AWS   bool   // paire de clés d'accès (credentials set-aws) plutôt qu'une simple valeur
}

// CredentialRefs liste les secrets dont la configuration a besoin, sans doublon.
func (c *Config) CredentialRefs() []CredentialRef {
	var refs []CredentialRef
	seen := map[string]bool{}
	add := func(field, value string, aws bool) {
		name, ok := strings.CutPrefix(value, CredentialPrefix)
		if !ok || name == "" || seen[name] {
			return
		}
		seen[name] = true
		refs = append(refs, CredentialRef{Name: name, Field: field, AWS: aws})
	}
	add("profile.aws_credentials", c.Profile.AWSCredentials, true)
	if r := c.Notify.Report; r != nil {
		add("notify.report.url", r.URL, false)
		add("notify.report.token", r.Token, false)
	}
	return refs
}

// Logging règle la journalisation.
type Logging struct {
	Level         string `yaml:"level"`
	RetentionDays int    `yaml:"retention_days"`
}

// Duration accepte "90s", "6h", "1h30m" dans le YAML.
type Duration struct{ time.Duration }

// UnmarshalYAML implémente yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("ligne %d : durée attendue (ex. 90s, 6h)", n.Line)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("ligne %d : durée invalide %q (ex. 90s, 6h)", n.Line, s)
	}
	d.Duration = v
	return nil
}

// MarshalYAML implémente yaml.Marshaler (utile à kura-ctl).
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// LoadOptions règle les vérifications faites au chargement.
type LoadOptions struct {
	// Env résout ${PROFILE:compte} et ${VAR}. Nil = environnement du système.
	Env Env
	// CheckSystem vérifie l'existence des sources et des comptes sur la machine.
	// kura-ctl le désactive : il génère des configs pour d'autres machines.
	CheckSystem bool
}

// Load lit, complète, développe et valide un fichier de configuration.
func Load(path string, opt LoadOptions) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lecture de la configuration : %w", err)
	}
	return Parse(data, opt)
}

// Parse fait comme Load à partir d'un contenu déjà lu.
func Parse(data []byte, opt LoadOptions) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // un champ inconnu est une faute de frappe, pas une option
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, errEOF) {
			return nil, errors.New("configuration vide")
		}
		return nil, fmt.Errorf("configuration illisible : %w", err)
	}
	c.ApplyDefaults()
	env := opt.Env
	if env == nil {
		env = SystemEnv()
	}
	if err := c.Expand(env); err != nil {
		return nil, err
	}
	if err := c.Validate(ValidateOptions{CheckSystem: opt.CheckSystem, Env: env}); err != nil {
		return nil, err
	}
	return &c, nil
}

// ApplyDefaults complète les valeurs absentes.
func (c *Config) ApplyDefaults() {
	if c.Schedule.MaxDuration.Duration == 0 {
		c.Schedule.MaxDuration.Duration = 6 * time.Hour
	}
	for i := range c.PreTasks {
		p := &c.PreTasks[i]
		if p.GracefulTimeout.Duration == 0 {
			p.GracefulTimeout.Duration = 60 * time.Second
		}
	}
	for i := range c.Jobs {
		j := &c.Jobs[i]
		if j.Name == "" {
			j.Name = j.ID
		}
		if j.Delete && j.Guard == nil {
			// Une source vide ou démontée ne doit jamais vider le bucket, même si
			// personne n'a pensé à configurer le garde-fou. Pas de seuil en
			// pourcentage par défaut : sur un petit dossier (Outlook, quelques
			// fichiers), une seule suppression légitime le dépasserait.
			j.Guard = &Guard{MinSourceFiles: 1}
		}
		if j.Delete && j.Guard.MinSourceFiles < 1 {
			// Même avec une section guard partielle (ex. seulement max_deletes),
			// une source vide ne doit jamais passer.
			j.Guard.MinSourceFiles = 1
		}
	}
	if w := c.Notify.Webhook; w != nil && w.URL != "" && c.Notify.Report == nil {
		c.Notify.Report = &Report{Type: ReportWebhook, URL: w.URL, Token: w.Token, HMAC: w.HMAC}
	}
	if c.Tray.LateAfter.Duration == 0 {
		c.Tray.LateAfter.Duration = 48 * time.Hour
	}
	if len(c.Notify.ToastOn) == 0 {
		c.Notify.ToastOn = []string{"failure", "warning"}
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.RetentionDays == 0 {
		c.Logging.RetentionDays = 90
	}
}

// Job renvoie le job d'identifiant id.
func (c *Config) Job(id string) (Job, bool) {
	for _, j := range c.Jobs {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

// PreTaskFor renvoie la pré-tâche qui ferme le processus donné.
func (c *Config) PreTaskFor(process string) (PreTask, bool) {
	for _, p := range c.PreTasks {
		if equalFold(p.Process, process) {
			return p, true
		}
	}
	return PreTask{}, false
}

// ReadTray lit la seule section tray, même si le reste du fichier ne passe pas la
// validation : l'icône doit savoir à qui s'afficher pour signaler l'erreur.
func ReadTray(path string) (Tray, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Tray{}, err
	}
	var doc struct {
		Tray Tray `yaml:"tray"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Tray{}, err
	}
	return doc.Tray, nil
}
