// Package launchd décrit la planification de kura-agent sous macOS : un démon
// launchd (LaunchDaemon) qui lance « kura-agent run --scheduled » aux heures de la
// section schedule de la configuration, sous root, puis l'installe ou le
// retire (launchctl).
//
// Même règle que la tâche Windows \Kura\Sync (schedtask.Next), traduite en
// StartCalendarInterval (une entrée par passage possible ; une clé absente vaut
// « toutes les valeurs ») :
//
//	daily    {Hour, Minute}, ou une entrée par jour de schedule.days
//	weekly   une entrée {Weekday, Hour, Minute} par jour de schedule.days
//	hourly   {Minute} ; avec schedule.days, une entrée par heure de chaque série de
//	         24 h (une série du lundi 22:30 court jusqu'au mardi 21:30)
//
// launchd rattrape un passage manqué pendant la veille (une seule fois, au réveil),
// pas un passage manqué machine éteinte. Avec run_if_missed, un second démon,
// lancé au démarrage (RunAtLoad), exécute « kura-agent run --scheduled --catch-up » :
// l'agent ne sauvegarde que si un passage prévu a été manqué (voir Missed).
//
// launchd n'a pas de durée maximale : l'agent s'arrête lui-même à
// schedule.max_duration. La génération ne dépend pas de macOS : elle se teste
// partout.
package launchd

import (
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/pkg/config"
)

// Démons installés.
const (
	// Label : sauvegarde planifiée.
	Label = "com.e-c-g.kura.sync"
	// CatchUpLabel : rattrapage au démarrage (run_if_missed).
	CatchUpLabel = "com.e-c-g.kura.catchup"
	// DaemonDir : démons installés par l'administrateur.
	DaemonDir = "/Library/LaunchDaemons"
)

// PlistName : fichier d'un démon dans DaemonDir.
func PlistName(label string) string { return label + ".plist" }

// Params complète la planification de la configuration.
type Params struct {
	Command string // chemin de kura-agent, ex. /usr/local/bin/kura-agent
	User    string // compte d'exécution ; vide : root (choix retenu sous macOS)
	DataDir string // dossier de données (HOME du démon, dossier de travail)
	LogsDir string // sortie et erreurs de launchd
}

// Plists : contenu des démons à installer. CatchUp est vide sans run_if_missed.
type Plists struct {
	Sync, CatchUp []byte
}

// Interval est une entrée de StartCalendarInterval ; -1 : toutes les valeurs.
type Interval struct {
	Weekday int // 0 dimanche … 6 samedi
	Hour    int
	Minute  int
}

var dayOrder = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func weekday(d string) int {
	for i, k := range dayOrder {
		if k == d {
			return i
		}
	}
	return -1
}

// Intervals traduit la planification en entrées StartCalendarInterval.
func Intervals(s config.Schedule) ([]Interval, error) {
	at := s.Time
	if at == "" && s.Type == "hourly" {
		at = "00:00"
	}
	hm, err := time.Parse("15:04", at)
	if err != nil {
		return nil, fmt.Errorf("schedule.time : heure HH:MM attendue, reçu %q", s.Time)
	}
	h, m := hm.Hour(), hm.Minute()
	seen := map[int]bool{}
	var days []int
	for _, d := range s.Days {
		n := weekday(d)
		if n < 0 {
			return nil, fmt.Errorf("schedule.days : jour inconnu dans %v", s.Days)
		}
		if !seen[n] {
			seen[n] = true
			days = append(days, n)
		}
	}
	sort.Ints(days)
	var out []Interval
	switch s.Type {
	case "daily", "weekly":
		if s.Type == "weekly" && len(days) == 0 {
			return nil, fmt.Errorf("schedule.days obligatoire pour weekly")
		}
		if len(days) == 0 || len(days) == 7 {
			return []Interval{{Weekday: -1, Hour: h, Minute: m}}, nil
		}
		for _, d := range days {
			out = append(out, Interval{Weekday: d, Hour: h, Minute: m})
		}
		return out, nil
	case "hourly":
		if len(days) == 0 || len(days) == 7 {
			return []Interval{{Weekday: -1, Hour: -1, Minute: m}}, nil
		}
		// Une série démarre le jour choisi à HH:MM et dure 24 h : heures h..23 de ce
		// jour, puis 0..h-1 du lendemain.
		hours := map[[2]int]bool{}
		for _, d := range days {
			for x := 0; x < 24; x++ {
				day, hour := d, h+x
				if hour >= 24 {
					day, hour = (d+1)%7, hour-24
				}
				hours[[2]int{day, hour}] = true
			}
		}
		for day := 0; day < 7; day++ {
			all := true
			for hour := 0; hour < 24; hour++ {
				all = all && hours[[2]int{day, hour}]
			}
			if all {
				out = append(out, Interval{Weekday: day, Hour: -1, Minute: m})
				continue
			}
			for hour := 0; hour < 24; hour++ {
				if hours[[2]int{day, hour}] {
					out = append(out, Interval{Weekday: day, Hour: hour, Minute: m})
				}
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("schedule.type : valeur %q inconnue", s.Type)
}

// Next renvoie le premier passage des entrées strictement après after (heure
// locale), comme launchd les lit ; zéro si aucune.
func Next(ivs []Interval, after time.Time) time.Time {
	c := after.Local().Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 8*24*60; i++ {
		for _, iv := range ivs {
			if (iv.Weekday < 0 || int(c.Weekday()) == iv.Weekday) && (iv.Hour < 0 || c.Hour() == iv.Hour) && c.Minute() == iv.Minute {
				return c
			}
		}
		c = c.Add(time.Minute)
	}
	return time.Time{}
}

// Generate renvoie les démons de la planification.
func Generate(s config.Schedule, p Params) (Plists, error) {
	for name, v := range map[string]string{"chemin de kura-agent": p.Command, "dossier de données": p.DataDir, "dossier des journaux": p.LogsDir} {
		if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, "\n\x00") {
			return Plists{}, fmt.Errorf("%s invalide : %q", name, v)
		}
	}
	if strings.ContainsAny(p.User, " \t\n\"'\\$%:/<>&") {
		return Plists{}, fmt.Errorf("compte d'exécution invalide : %q", p.User)
	}
	ivs, err := Intervals(s)
	if err != nil {
		return Plists{}, err
	}
	var cal strings.Builder
	cal.WriteString("\t<key>StartCalendarInterval</key>\n\t<array>\n")
	for _, iv := range ivs {
		cal.WriteString("\t\t<dict>\n")
		if iv.Weekday >= 0 {
			fmt.Fprintf(&cal, "\t\t\t<key>Weekday</key>\n\t\t\t<integer>%d</integer>\n", iv.Weekday)
		}
		if iv.Hour >= 0 {
			fmt.Fprintf(&cal, "\t\t\t<key>Hour</key>\n\t\t\t<integer>%d</integer>\n", iv.Hour)
		}
		fmt.Fprintf(&cal, "\t\t\t<key>Minute</key>\n\t\t\t<integer>%d</integer>\n", iv.Minute)
		cal.WriteString("\t\t</dict>\n")
	}
	cal.WriteString("\t</array>\n")
	out := Plists{Sync: plist(Label, p, []string{"run", "--scheduled"}, "sync", cal.String())}
	if s.RunIfMissed {
		out.CatchUp = plist(CatchUpLabel, p, []string{"run", "--scheduled", "--catch-up"}, "catchup", "\t<key>RunAtLoad</key>\n\t<true/>\n")
	}
	return out, nil
}

func plist(label string, p Params, args []string, logName, trigger string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Géré par kura-agent install / uninstall : ne pas modifier à la main. -->
<plist version="1.0">
<dict>
`)
	kv := func(k, v string) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", k, esc(v)) }
	kv("Label", label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range append([]string{p.Command}, args...) {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", esc(a))
	}
	b.WriteString("\t</array>\n")
	if p.User != "" {
		kv("UserName", p.User)
		kv("GroupName", p.User)
	}
	kv("WorkingDirectory", p.DataDir)
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	fmt.Fprintf(&b, "\t\t<key>HOME</key>\n\t\t<string>%s</string>\n", esc(p.DataDir))
	b.WriteString("\t\t<key>PATH</key>\n\t\t<string>/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>\n")
	b.WriteString("\t</dict>\n")
	b.WriteString(trigger)
	// Sortie de l'agent (son journal détaillé est dans les fichiers de logs).
	kv("StandardOutPath", p.LogsDir+"/launchd-"+logName+".log")
	kv("StandardErrorPath", p.LogsDir+"/launchd-"+logName+".log")
	// Priorité basse, comme sous Linux (Nice=10, E/S au repos) ; pas de ProcessType
	// Background, qui bride trop les envois longs.
	b.WriteString("\t<key>LowPriorityIO</key>\n\t<true/>\n\t<key>Nice</key>\n\t<integer>10</integer>\n")
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
