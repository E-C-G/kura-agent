package schedtask

import (
	"time"

	"github.com/E-C-G/kura-agent/pkg/config"
)

// Next renvoie le prochain déclenchement de la planification strictement après
// after, en heure locale, ou l'instant zéro si la planification est invalide.
// Même règle que la tâche générée par XML : à l'heure dite les jours choisis ;
// pour hourly, toutes les heures à partir de l'heure dite, pendant 24 h.
func Next(s config.Schedule, after time.Time) time.Time {
	at := s.Time
	if at == "" {
		at = "00:00"
	}
	hm, err := time.Parse("15:04", at)
	if err != nil {
		return time.Time{}
	}
	allowed := func(d time.Time) bool {
		if len(s.Days) == 0 {
			return s.Type != "weekly"
		}
		day := d.Weekday()
		for _, k := range s.Days {
			if n, ok := weekdayNum[k]; ok && n == day {
				return true
			}
		}
		return false
	}
	after = after.Local()
	switch s.Type {
	case "daily", "weekly":
		for i := 0; i <= 8; i++ {
			d := after.AddDate(0, 0, i)
			c := time.Date(d.Year(), d.Month(), d.Day(), hm.Hour(), hm.Minute(), 0, 0, time.Local)
			if c.After(after) && allowed(c) {
				return c
			}
		}
	case "hourly":
		// Premier passage à la minute dite après after, puis d'heure en heure. Un passage
		// appartient au jour où sa série de 24 h a démarré (heure dite incluse).
		c := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), hm.Minute(), 0, 0, time.Local)
		for !c.After(after) {
			c = c.Add(time.Hour)
		}
		for i := 0; i < 9*24; i++ {
			start := c.Add(-time.Duration(hm.Hour())*time.Hour - time.Duration(hm.Minute())*time.Minute)
			if allowed(start) {
				return c
			}
			c = c.Add(time.Hour)
		}
	}
	return time.Time{}
}

var weekdayNum = map[string]time.Weekday{"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday}

// Prev renvoie le dernier déclenchement de la planification au plus tard à at, ou
// l'instant zéro s'il n'y en a pas dans les 8 jours qui précèdent.
func Prev(s config.Schedule, at time.Time) time.Time {
	var last time.Time
	for c := Next(s, at.Add(-8*24*time.Hour)); !c.IsZero() && !c.After(at); c = Next(s, c) {
		last = c
	}
	return last
}

// Missed indique si un déclenchement prévu au plus tard à now n'a pas eu lieu :
// postérieur au dernier lancement (lastStart) et à la mise en place de la
// planification (since). Sert au rattrapage au démarrage sous macOS.
func Missed(s config.Schedule, lastStart, since, now time.Time) bool {
	p := Prev(s, now)
	return !p.IsZero() && p.After(lastStart) && p.After(since)
}
