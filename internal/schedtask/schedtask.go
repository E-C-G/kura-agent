// Package schedtask décrit la tâche planifiée \Kura\Sync au format XML du
// Planificateur de tâches Windows, puis l'enregistre ou la supprime (schtasks.exe).
//
// La génération du XML ne dépend pas de Windows : elle se teste partout.
package schedtask

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/E-C-G/kura-agent/pkg/config"
)

// Name est le chemin de la tâche dans le Planificateur.
const Name = `\Kura\Sync`

// Margin s'ajoute à schedule.max_duration pour la limite du Planificateur : l'agent
// s'arrête lui-même à max_duration en écrivant son bilan ; Windows ne tue le
// processus qu'en dernier recours.
const Margin = 15 * time.Minute

// SystemSID est le compte SYSTEM (NT AUTHORITY\SYSTEM).
const SystemSID = "S-1-5-18"

// Params complète la planification de la configuration.
type Params struct {
	UserID     string    // compte qui exécute la tâche : SID ; défaut SYSTEM
	Command    string    // chemin de kura-agent.exe
	Arguments  string    // ex. "run --quiet"
	WorkingDir string    // ex. C:\ProgramData\Kura
	RunNowSIDs []string  // comptes autorisés à lancer la tâche (tray.allow_run_now)
	Now        time.Time // date de départ des déclencheurs ; défaut : aujourd'hui
}

// XML renvoie la définition de la tâche.
//
// Principal : SYSTEM (compte de service, aucun mot de passe), ou un autre compte en
// ouverture de session S4U ; autorisations maximales dans les deux cas.
func XML(s config.Schedule, p Params) (string, error) {
	if p.Command == "" {
		return "", fmt.Errorf("commande obligatoire")
	}
	if p.UserID == "" {
		p.UserID = SystemSID
	}
	// Dans le XML, une tâche SYSTEM n'a pas de LogonType (c'est ainsi que Windows
	// l'exporte) : « ServiceAccount » n'est qu'une valeur de l'API COM, refusée ici.
	logon := "S4U"
	if p.UserID == SystemSID {
		logon = ""
	}
	trig, err := trigger(s, p.Now)
	if err != nil {
		return "", err
	}
	sd := "D:(A;;FA;;;SY)(A;;FA;;;BA)"
	for _, sid := range p.RunNowSIDs {
		if !strings.HasPrefix(sid, "S-1-") || strings.ContainsAny(sid, "();") {
			return "", fmt.Errorf("SID invalide %q", sid)
		}
		sd += "(A;;FRFX;;;" + sid + ")"
	}
	limit := s.MaxDuration.Duration
	if limit <= 0 {
		limit = 6 * time.Hour
	}
	t := task{
		Version: "1.4",
		NS:      "http://schemas.microsoft.com/windows/2004/02/mit/task",
		RegistrationInfo: registrationInfo{
			Author:             "kura-agent",
			Description:        "Sauvegarde Kura vers Wasabi. Gérée par kura-agent install / uninstall : ne pas modifier à la main.",
			URI:                Name,
			SecurityDescriptor: sd,
		},
		Triggers: triggers{Calendar: trig},
		Principals: principals{Principal: principal{
			ID: "Author", UserID: p.UserID, LogonType: logon, RunLevel: "HighestAvailable",
		}},
		Settings: settings{
			MultipleInstancesPolicy:    "IgnoreNew",
			DisallowStartIfOnBatteries: false,
			StopIfGoingOnBatteries:     false,
			AllowHardTerminate:         true,
			StartWhenAvailable:         s.RunIfMissed,
			RunOnlyIfNetworkAvailable:  false,
			IdleSettings:               idleSettings{StopOnIdleEnd: false, RestartOnIdle: false},
			AllowStartOnDemand:         true,
			Enabled:                    true,
			Hidden:                     false,
			RunOnlyIfIdle:              false,
			WakeToRun:                  false,
			ExecutionTimeLimit:         isoDuration(limit + Margin),
			Priority:                   7,
		},
		Actions: actions{Context: "Author", Exec: execAction{
			Command: p.Command, Arguments: p.Arguments, WorkingDirectory: p.WorkingDir,
		}},
	}
	out, err := xml.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", err
	}
	return `<?xml version="1.0" encoding="UTF-16"?>` + "\n" + string(out) + "\n", nil
}

// RunNowNotAppliedError : la tâche est créée, mais sans le descripteur de sécurité qui
// autorise les comptes de tray.allow_run_now à la lancer (refusé par schtasks).
// Les administrateurs peuvent toujours la lancer.
type RunNowNotAppliedError struct{ Cause string }

func (e *RunNowNotAppliedError) Error() string {
	return "tâche créée sans le droit de lancement pour tray.allow_run_now (" + e.Cause + ")"
}

// WithoutSecurityDescriptor retire l'élément SecurityDescriptor du XML.
func WithoutSecurityDescriptor(def string) string {
	start := strings.Index(def, "<SecurityDescriptor>")
	end := strings.Index(def, "</SecurityDescriptor>")
	if start < 0 || end < start {
		return def
	}
	end += len("</SecurityDescriptor>")
	// La ligne entière, indentation et fin de ligne comprises.
	for start > 0 && (def[start-1] == ' ' || def[start-1] == '\t') {
		start--
	}
	if end < len(def) && def[end] == '\n' {
		end++
	}
	return def[:start] + def[end:]
}

// UTF16 encode le XML comme l'attend schtasks.exe : UTF-16 LE avec BOM.
func UTF16(s string) []byte {
	u := utf16.Encode([]rune(strings.ReplaceAll(s, "\n", "\r\n")))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for i, c := range u {
		b[2+2*i], b[3+2*i] = byte(c), byte(c>>8)
	}
	return b
}

var weekdays = map[string]string{"mon": "Monday", "tue": "Tuesday", "wed": "Wednesday",
	"thu": "Thursday", "fri": "Friday", "sat": "Saturday", "sun": "Sunday"}

func trigger(s config.Schedule, now time.Time) (*calendarTrigger, error) {
	if now.IsZero() {
		now = time.Now()
	}
	at := s.Time
	if at == "" {
		if s.Type != "hourly" {
			return nil, fmt.Errorf("schedule.time obligatoire pour %q", s.Type)
		}
		at = "00:00"
	}
	hm, err := time.Parse("15:04", at)
	if err != nil {
		return nil, fmt.Errorf("schedule.time %q : HH:MM attendu", at)
	}
	// Heure locale sans fuseau : le déclenchement suit l'heure du poste, été comme hiver.
	start := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, time.Local)
	ct := &calendarTrigger{StartBoundary: start.Format("2006-01-02T15:04:05"), Enabled: true}

	var days *daysOfWeek
	if len(s.Days) > 0 {
		days = &daysOfWeek{}
		for _, d := range s.Days {
			name, ok := weekdays[d]
			if !ok {
				return nil, fmt.Errorf("schedule.days : jour %q inconnu", d)
			}
			days.set(name)
		}
	}
	switch s.Type {
	case "daily", "hourly":
		if s.Type == "hourly" {
			ct.Repetition = &repetition{Interval: "PT1H", Duration: "P1D", StopAtDurationEnd: false}
		}
		if days != nil {
			ct.ByWeek = &scheduleByWeek{DaysOfWeek: *days, WeeksInterval: 1}
		} else {
			ct.ByDay = &scheduleByDay{DaysInterval: 1}
		}
	case "weekly":
		if days == nil {
			return nil, fmt.Errorf("schedule.days obligatoire pour weekly")
		}
		ct.ByWeek = &scheduleByWeek{DaysOfWeek: *days, WeeksInterval: 1}
	default:
		return nil, fmt.Errorf("schedule.type %q inconnu", s.Type)
	}
	return ct, nil
}

// isoDuration écrit une durée au format ISO 8601 attendu par le Planificateur (PT6H15M).
func isoDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)
	out := "PT"
	if h > 0 {
		out += fmt.Sprintf("%dH", h)
	}
	if m > 0 {
		out += fmt.Sprintf("%dM", m)
	}
	if s > 0 || out == "PT" {
		out += fmt.Sprintf("%dS", s)
	}
	return out
}

// Structures du schéma du Planificateur ; l'ordre des champs est celui des exports Windows.

type task struct {
	XMLName          xml.Name         `xml:"Task"`
	Version          string           `xml:"version,attr"`
	NS               string           `xml:"xmlns,attr"`
	RegistrationInfo registrationInfo `xml:"RegistrationInfo"`
	Triggers         triggers         `xml:"Triggers"`
	Principals       principals       `xml:"Principals"`
	Settings         settings         `xml:"Settings"`
	Actions          actions          `xml:"Actions"`
}

type registrationInfo struct {
	Author             string `xml:"Author"`
	Description        string `xml:"Description"`
	URI                string `xml:"URI"`
	SecurityDescriptor string `xml:"SecurityDescriptor"`
}

type triggers struct {
	Calendar *calendarTrigger `xml:"CalendarTrigger"`
}

type calendarTrigger struct {
	Repetition    *repetition     `xml:"Repetition,omitempty"`
	StartBoundary string          `xml:"StartBoundary"`
	Enabled       bool            `xml:"Enabled"`
	ByDay         *scheduleByDay  `xml:"ScheduleByDay,omitempty"`
	ByWeek        *scheduleByWeek `xml:"ScheduleByWeek,omitempty"`
}

type repetition struct {
	Interval          string `xml:"Interval"`
	Duration          string `xml:"Duration"`
	StopAtDurationEnd bool   `xml:"StopAtDurationEnd"`
}

type scheduleByDay struct {
	DaysInterval int `xml:"DaysInterval"`
}

type scheduleByWeek struct {
	DaysOfWeek    daysOfWeek `xml:"DaysOfWeek"`
	WeeksInterval int        `xml:"WeeksInterval"`
}

type empty struct{}

type daysOfWeek struct {
	Monday    *empty `xml:"Monday,omitempty"`
	Tuesday   *empty `xml:"Tuesday,omitempty"`
	Wednesday *empty `xml:"Wednesday,omitempty"`
	Thursday  *empty `xml:"Thursday,omitempty"`
	Friday    *empty `xml:"Friday,omitempty"`
	Saturday  *empty `xml:"Saturday,omitempty"`
	Sunday    *empty `xml:"Sunday,omitempty"`
}

func (d *daysOfWeek) set(name string) {
	e := &empty{}
	switch name {
	case "Monday":
		d.Monday = e
	case "Tuesday":
		d.Tuesday = e
	case "Wednesday":
		d.Wednesday = e
	case "Thursday":
		d.Thursday = e
	case "Friday":
		d.Friday = e
	case "Saturday":
		d.Saturday = e
	case "Sunday":
		d.Sunday = e
	}
}

type principals struct {
	Principal principal `xml:"Principal"`
}

type principal struct {
	ID        string `xml:"id,attr"`
	UserID    string `xml:"UserId"`
	LogonType string `xml:"LogonType,omitempty"`
	RunLevel  string `xml:"RunLevel"`
}

type idleSettings struct {
	StopOnIdleEnd bool `xml:"StopOnIdleEnd"`
	RestartOnIdle bool `xml:"RestartOnIdle"`
}

type settings struct {
	MultipleInstancesPolicy    string       `xml:"MultipleInstancesPolicy"`
	DisallowStartIfOnBatteries bool         `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     bool         `xml:"StopIfGoingOnBatteries"`
	AllowHardTerminate         bool         `xml:"AllowHardTerminate"`
	StartWhenAvailable         bool         `xml:"StartWhenAvailable"`
	RunOnlyIfNetworkAvailable  bool         `xml:"RunOnlyIfNetworkAvailable"`
	IdleSettings               idleSettings `xml:"IdleSettings"`
	AllowStartOnDemand         bool         `xml:"AllowStartOnDemand"`
	Enabled                    bool         `xml:"Enabled"`
	Hidden                     bool         `xml:"Hidden"`
	RunOnlyIfIdle              bool         `xml:"RunOnlyIfIdle"`
	WakeToRun                  bool         `xml:"WakeToRun"`
	ExecutionTimeLimit         string       `xml:"ExecutionTimeLimit"`
	Priority                   int          `xml:"Priority"`
}

type actions struct {
	Context string     `xml:"Context,attr"`
	Exec    execAction `xml:"Exec"`
}

type execAction struct {
	Command          string `xml:"Command"`
	Arguments        string `xml:"Arguments,omitempty"`
	WorkingDirectory string `xml:"WorkingDirectory,omitempty"`
}
