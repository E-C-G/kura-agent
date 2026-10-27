// Package systemd décrit la planification de kura-agent sous Linux : un service
// kura-sync.service (une exécution de « kura-agent run », déroulé aussi dans le
// journal) déclenché par un timer
// kura-sync.timer, générés depuis la section schedule de la configuration, puis
// les installe ou les retire (systemctl).
//
// Même règle que la tâche Windows \Kura\Sync (schedtask.Next) :
//
//	daily    à l'heure dite, tous les jours ou les jours de schedule.days
//	weekly   à l'heure dite, les jours de schedule.days
//	hourly   toutes les heures à la minute dite, par séries de 24 h qui démarrent à
//	         l'heure dite les jours choisis (une série du lundi 22:30 court jusqu'au
//	         mardi 21:30)
//
// run_if_missed devient Persistent=true (rattrapage au démarrage suivant) ;
// max_duration + 15 min devient la limite de systemd, l'agent s'arrêtant lui-même à
// max_duration. La génération ne dépend pas de Linux : elle se teste partout.
package systemd

import (
	"fmt"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/pkg/config"
)

// Noms des unités.
const (
	ServiceName = "kura-sync.service"
	TimerName   = "kura-sync.timer"
	// UnitDir : unités installées par l'administrateur.
	UnitDir = "/etc/systemd/system"
	// Margin s'ajoute à schedule.max_duration (comme sous Windows).
	Margin = 15 * time.Minute
)

// Params complète la planification de la configuration.
type Params struct {
	Command string // chemin de kura-agent, ex. /usr/bin/kura-agent
	User    string // compte d'exécution, ex. kura-agent
}

// Units : contenu des deux fichiers d'unité.
type Units struct {
	Service, Timer string
}

var calendarDays = map[string]string{"mon": "Mon", "tue": "Tue", "wed": "Wed", "thu": "Thu", "fri": "Fri", "sat": "Sat", "sun": "Sun"}

var dayOrder = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// OnCalendar traduit la planification en expressions OnCalendar de systemd (une ou
// plusieurs : le timer se déclenche à chacune).
func OnCalendar(s config.Schedule) ([]string, error) {
	at := s.Time
	if at == "" && s.Type == "hourly" {
		at = "00:00"
	}
	hm, err := time.Parse("15:04", at)
	if err != nil {
		return nil, fmt.Errorf("schedule.time : heure HH:MM attendue, reçu %q", s.Time)
	}
	h, m := hm.Hour(), hm.Minute()
	var days []string
	for _, d := range dayOrder {
		for _, k := range s.Days {
			if k == d {
				days = append(days, d)
				break
			}
		}
	}
	if len(days) != len(s.Days) {
		return nil, fmt.Errorf("schedule.days : jour inconnu dans %v", s.Days)
	}
	list := func(ds []string) string {
		out := make([]string, len(ds))
		for i, d := range ds {
			out[i] = calendarDays[d]
		}
		return strings.Join(out, ",")
	}
	switch s.Type {
	case "daily", "weekly":
		if s.Type == "weekly" && len(days) == 0 {
			return nil, fmt.Errorf("schedule.days obligatoire pour weekly")
		}
		prefix := ""
		if len(days) > 0 {
			prefix = list(days) + " "
		}
		return []string{fmt.Sprintf("%s*-*-* %02d:%02d:00", prefix, h, m)}, nil
	case "hourly":
		if len(days) == 0 || len(days) == 7 {
			return []string{fmt.Sprintf("*-*-* *:%02d:00", m)}, nil
		}
		if h == 0 {
			return []string{fmt.Sprintf("%s *-*-* *:%02d:00", list(days), m)}, nil
		}
		// Une série démarre le jour choisi à HH:MM et finit le lendemain avant HH:MM.
		next := make([]string, len(days))
		for i, d := range days {
			next[i] = dayOrder[(indexOf(d)+1)%7]
		}
		return []string{
			fmt.Sprintf("%s *-*-* %02d..23:%02d:00", list(days), h, m),
			fmt.Sprintf("%s *-*-* 00..%02d:%02d:00", list(sortDays(next)), h-1, m),
		}, nil
	}
	return nil, fmt.Errorf("schedule.type : valeur %q inconnue", s.Type)
}

func indexOf(d string) int {
	for i, k := range dayOrder {
		if k == d {
			return i
		}
	}
	return -1
}

func sortDays(ds []string) []string {
	var out []string
	for _, d := range dayOrder {
		for _, k := range ds {
			if k == d {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// Generate renvoie le service et le timer.
func Generate(s config.Schedule, p Params) (Units, error) {
	if p.Command == "" || !strings.HasPrefix(p.Command, "/") || strings.ContainsAny(p.Command, " \t\n\"'\\$%") {
		return Units{}, fmt.Errorf("chemin de kura-agent invalide : %q", p.Command)
	}
	if p.User == "" || strings.ContainsAny(p.User, " \t\n\"'\\$%:/") {
		return Units{}, fmt.Errorf("compte d'exécution invalide : %q", p.User)
	}
	cals, err := OnCalendar(s)
	if err != nil {
		return Units{}, err
	}
	limit := s.MaxDuration.Duration
	if limit <= 0 {
		limit = 6 * time.Hour
	}
	var svc strings.Builder
	fmt.Fprintf(&svc, `# Géré par kura-agent install / uninstall : ne pas modifier à la main.
[Unit]
Description=Kura : sauvegarde vers Wasabi
Documentation=https://github.com/E-C-G/kura-agent
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
# Sans --quiet : le déroulé va aussi dans le journal (journalctl -u kura-sync).
ExecStart=%s run --scheduled
# Code 1 : avertissement (fichiers ignorés), pas un échec du service.
SuccessExitStatus=1
TimeoutStartSec=%d
User=%s
Group=%s
Environment=HOME=/var/lib/kura
# Secrets : /etc/credstore.encrypted/kura.*, déchiffrés par systemd dans
# $CREDENTIALS_DIRECTORY, lisibles par ce service seul.
ImportCredential=kura.*
# Lecture de tout le disque, sans écriture : capacité transmise à aws.
AmbientCapabilities=CAP_DAC_READ_SEARCH
CapabilityBoundingSet=CAP_DAC_READ_SEARCH
NoNewPrivileges=yes
# Écriture limitée à /var/lib/kura et /var/log/kura ; /etc/kura en lecture. État et
# journaux lisibles par tous (icône, kura-agent status), comme sous Windows : aucun
# secret n'y est rangé.
StateDirectory=kura
StateDirectoryMode=0755
LogsDirectory=kura
LogsDirectoryMode=0755
ConfigurationDirectory=kura
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
Nice=10
IOSchedulingClass=idle
`, p.Command, int((limit + Margin).Seconds()), p.User, p.User)

	var tim strings.Builder
	tim.WriteString(`# Géré par kura-agent install / uninstall : ne pas modifier à la main.
[Unit]
Description=Kura : planification de la sauvegarde

[Timer]
Unit=` + ServiceName + "\n")
	for _, c := range cals {
		tim.WriteString("OnCalendar=" + c + "\n")
	}
	fmt.Fprintf(&tim, "Persistent=%s\nAccuracySec=1min\n\n[Install]\nWantedBy=timers.target\n", yesNo(s.RunIfMissed))
	return Units{Service: svc.String(), Timer: tim.String()}, nil
}

func yesNo(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
