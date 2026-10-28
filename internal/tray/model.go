// Package tray est l'icône de Kura dans la barre de notification de chaque session.
//
// model.go (portable, testé partout) calcule ce que l'icône affiche à partir de
// status.json, suspend.json et de la configuration ; l'interface Win32 est dans
// les fichiers *_windows.go.
package tray

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/schedtask"
	"github.com/E-C-G/kura-agent/internal/selfupdate"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Icon est l'état affiché.
type Icon int

const (
	IconUnknown   Icon = iota // aucune sauvegarde enregistrée
	IconOK                    // nuage + coche verte
	IconRunning               // nuage + flèche bleue
	IconWarning               // nuage + point orange
	IconError                 // nuage + croix rouge
	IconSuspended             // nuage gris
	IconLate                  // nuage + horloge orange
)

// Input rassemble ce que l'icône lit.
type Input struct {
	Status     *state.Status
	StatusErr  error
	Config     *config.Config // nil si illisible
	ConfigErr  error
	Suspension *state.Suspension
	User       string // DOMAINE\nom de la session
	Connected  bool   // run est connecté à cette icône (annulation possible)
	Now        time.Time

	// Mise à jour de l'agent (kura-agent self-update).
	Version  string            // version de l'icône (celle de l'agent installé)
	Admin    bool              // compte membre du groupe Administrateurs du poste
	Update   selfupdate.Status // update.json
	Updating string            // étape d'une mise à jour en cours, signalée par self-update
}

// View est ce que l'icône affiche.
type View struct {
	Icon     Icon
	Tooltip  string // 127 caractères au plus (limite de Windows)
	LastLine string // « Dernière sauvegarde : OK – 24/10 22:47 »
	NextLine string // « Prochaine : 25/10 22:30 »

	Running       bool
	Suspended     bool
	ShowRunNow    bool // compte de tray.allow_run_now
	RunNowEnabled bool
	ShowCancel    bool
	ShowRestore   bool // « Restaurer mes fichiers… » : comptes de tray.users

	// Mise à jour : réservée aux administrateurs du poste.
	UpdateTo        string // version plus récente disponible, attestée
	UpdateLabel     string // « Mettre à jour vers 0.11.0… »
	UpdateEnabled   bool
	ShowCheckUpdate bool // « Rechercher une mise à jour »
}

const dateFmt = "02/01 15:04"

// Compute calcule l'affichage. Ordre de priorité : en cours, configuration invalide,
// échec, suspendu, retard, avertissement, OK.
func Compute(in Input) View {
	v := View{}
	st := in.Status
	allowed := in.Config != nil && config.Listed(in.Config.Tray.AllowRunNow, in.User)
	v.Running = st != nil && st.State == "running"
	v.Suspended = in.Suspension.Active(in.Now)

	v.LastLine = "Dernière sauvegarde : aucune"
	if st != nil && st.LastSuccess != nil {
		v.LastLine = "Dernière sauvegarde réussie : " + st.LastSuccess.Local().Format(dateFmt)
	}
	switch {
	case v.Suspended:
		v.NextLine = "Suspendue jusqu'au " + in.Suspension.Until.Local().Format(dateFmt)
	case in.Config != nil:
		if n := schedtask.Next(in.Config.Schedule, in.Now); !n.IsZero() {
			v.NextLine = "Prochaine : " + n.Format(dateFmt)
		}
	}

	switch {
	case in.Updating != "":
		v.Icon = IconRunning
		v.Tooltip = "Mise à jour de l'agent : " + in.Updating
	case v.Running:
		v.Icon = IconRunning
		v.Tooltip = "Sauvegarde en cours"
		if st.DryRun {
			v.Tooltip = "Simulation en cours"
		}
		if c := st.Current; c != nil {
			v.Tooltip += fmt.Sprintf(" : %s (%d/%d)", c.Job, c.Index, c.Total)
		}
	case in.ConfigErr != nil:
		v.Icon = IconError
		v.Tooltip = "Configuration invalide : " + firstLine(in.ConfigErr.Error())
	case st != nil && (st.Result == state.ResultError || st.Result == state.ResultGuard):
		v.Icon = IconError
		v.Tooltip = fmt.Sprintf("Échec le %s : %s", endOf(st).Format(dateFmt), reason(st))
	case v.Suspended:
		v.Icon = IconSuspended
		v.Tooltip = "Planification suspendue jusqu'au " + in.Suspension.Until.Local().Format(dateFmt)
	case late(in):
		v.Icon = IconLate
		v.Tooltip = fmt.Sprintf("Aucune sauvegarde réussie depuis plus de %s", human(lateAfter(in)))
	case st != nil && st.Result != "" && st.Result != state.ResultOK:
		v.Icon = IconWarning
		v.Tooltip = fmt.Sprintf("%s : %s", endOf(st).Format(dateFmt), reason(st))
	case st != nil && st.Result == state.ResultOK:
		v.Icon = IconOK
		v.Tooltip = "Dernière sauvegarde réussie le " + endOf(st).Format(dateFmt)
		if st.DryRun {
			v.Tooltip = "Simulation réussie le " + endOf(st).Format(dateFmt)
		}
	default:
		v.Icon = IconUnknown
		v.Tooltip = "Aucune sauvegarde enregistrée"
	}
	v.Tooltip = truncate("Kura – "+v.Tooltip, 127)

	v.ShowRunNow = allowed
	v.ShowRestore = in.Config != nil && config.Listed(in.Config.Tray.Users, in.User)
	v.RunNowEnabled = allowed && !v.Running && !v.Suspended && in.Updating == ""
	v.ShowCancel = allowed && v.Running && in.Connected
	if in.Admin {
		if to := newerAvailable(in); to != "" {
			v.UpdateTo = to
			v.UpdateLabel = "Mettre à jour vers " + to + "…"
			v.UpdateEnabled = !v.Running && in.Updating == ""
			if v.Running {
				v.UpdateLabel += " (après la sauvegarde en cours)"
			}
		} else {
			v.ShowCheckUpdate = in.Updating == ""
		}
	}
	return v
}

// newerAvailable : version notée par la recherche planifiée, plus récente que celle
// de l'icône (une mise à jour faite entre-temps la rend caduque).
func newerAvailable(in Input) string {
	a := in.Update.Available
	if a == nil {
		return ""
	}
	if cmp, err := selfupdate.Compare(a.Version, in.Version); err != nil || cmp <= 0 {
		return ""
	}
	return a.Version
}

// UpdatedToast : notification de l'icône relancée juste après une mise à jour réussie.
func UpdatedToast(u selfupdate.Status, version string, now time.Time) *Toast {
	l := u.Last
	if l == nil || l.Result != "ok" || l.To != version || now.Sub(l.At) > 5*time.Minute || now.Before(l.At) {
		return nil
	}
	return &Toast{Title: "Kura mis à jour", Text: fmt.Sprintf("Version %s installée (précédente : %s).", l.To, l.From), Level: LevelInfo}
}

func lateAfter(in Input) time.Duration {
	if in.Config != nil && in.Config.Tray.LateAfter.Duration > 0 {
		return in.Config.Tray.LateAfter.Duration
	}
	return 48 * time.Hour
}

// late : aucune réussite depuis LateAfter. Sans aucune réussite enregistrée, on compte
// depuis la première exécution connue.
func late(in Input) bool {
	st := in.Status
	if st == nil {
		return false
	}
	ref := st.StartedAt
	if st.LastSuccess != nil {
		ref = *st.LastSuccess
	}
	return in.Now.Sub(ref) > lateAfter(in)
}

func endOf(st *state.Status) time.Time {
	if st.EndedAt != nil {
		return st.EndedAt.Local()
	}
	return st.StartedAt.Local()
}

// reason résume ce qui n'a pas été : message global, sinon premier job en défaut.
func reason(st *state.Status) string {
	if st.Message != "" {
		return firstLine(st.Message)
	}
	for _, j := range st.Jobs {
		if j.Result != state.ResultOK {
			if j.Message != "" {
				return j.Name + " – " + firstLine(j.Message)
			}
			return j.Name + " – " + j.Result
		}
	}
	for _, p := range st.PreTasks {
		if p.Result != state.ResultOK && p.Message != "" {
			return p.Process + " – " + firstLine(p.Message)
		}
	}
	return st.Result
}

// Toast est une notification Windows.
type Toast struct {
	Title   string
	Text    string
	Level   int  // 1 information, 2 avertissement, 3 erreur
	OpenLog bool // un clic ouvre le journal de l'exécution
}

// Niveaux de toast.
const (
	LevelInfo    = 1
	LevelWarning = 2
	LevelError   = 3
)

// FinishedToast renvoie la notification de fin d'exécution à afficher en passant de
// prev à cur, ou nil. Rien au premier chargement (prev nil) ni pour une simulation.
func FinishedToast(prev, cur *state.Status, toastOn []string) *Toast {
	if prev == nil || cur == nil || cur.State != "idle" || cur.EndedAt == nil || cur.DryRun {
		return nil
	}
	if prev.RunID == cur.RunID && prev.State == "idle" {
		return nil // déjà vue
	}
	var t Toast
	var kind string
	switch cur.Result {
	case state.ResultOK:
		kind, t = "success", Toast{Title: "Sauvegarde réussie", Level: LevelInfo}
	case state.ResultError:
		kind, t = "failure", Toast{Title: "Échec de la sauvegarde", Level: LevelError, OpenLog: true}
	case state.ResultGuard:
		kind, t = "failure", Toast{Title: "Sauvegarde bloquée par le garde-fou", Level: LevelError, OpenLog: true}
	case state.ResultCancelled:
		kind, t = "warning", Toast{Title: "Sauvegarde annulée", Level: LevelWarning, OpenLog: true}
	default:
		kind, t = "warning", Toast{Title: "Sauvegarde terminée avec des avertissements", Level: LevelWarning, OpenLog: true}
	}
	if !slices.Contains(toastOn, kind) {
		return nil
	}
	if kind == "success" {
		t.Text = fmt.Sprintf("Terminée le %s.", endOf(cur).Format(dateFmt))
	} else {
		t.Text = truncate(reason(cur), 250)
	}
	return &t
}

// LateToast indique s'il faut prévenir du retard : au démarrage, puis une fois par jour.
func LateToast(v View, lastShown, now time.Time) *Toast {
	if v.Icon != IconLate || (!lastShown.IsZero() && now.Sub(lastShown) < 24*time.Hour) {
		return nil
	}
	return &Toast{Title: "Sauvegarde en retard", Text: strings.TrimPrefix(v.Tooltip, "Kura – "), Level: LevelWarning}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func human(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d jours", int(d/(24*time.Hour)))
	}
	return fmt.Sprintf("%d h", int(d/time.Hour))
}
