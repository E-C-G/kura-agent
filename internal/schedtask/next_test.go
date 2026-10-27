package schedtask

import (
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/pkg/config"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	week := []string{"mon", "tue", "wed", "thu", "fri"}
	for _, c := range []struct {
		s     config.Schedule
		after string
		want  string
	}{
		// 26/10/2026 est un lundi.
		{config.Schedule{Type: "daily", Time: "22:30", Days: week}, "2026-10-26 09:00", "2026-10-26 22:30"},
		{config.Schedule{Type: "daily", Time: "22:30", Days: week}, "2026-10-26 22:30", "2026-10-27 22:30"},
		{config.Schedule{Type: "daily", Time: "22:30", Days: week}, "2026-10-30 23:00", "2026-11-02 22:30"},
		{config.Schedule{Type: "daily", Time: "01:00"}, "2026-10-31 02:00", "2026-11-01 01:00"},
		{config.Schedule{Type: "weekly", Time: "20:00", Days: []string{"sat"}}, "2026-10-26 09:00", "2026-10-31 20:00"},
		{config.Schedule{Type: "hourly"}, "2026-10-26 09:10", "2026-10-26 10:00"},
		{config.Schedule{Type: "hourly", Time: "08:15"}, "2026-10-26 09:10", "2026-10-26 09:15"},
		// Lundi à vendredi à partir de 08:00 : la série du vendredi court jusqu'au samedi 07:00.
		{config.Schedule{Type: "hourly", Time: "08:00", Days: week}, "2026-10-31 06:30", "2026-10-31 07:00"},
		{config.Schedule{Type: "hourly", Time: "08:00", Days: week}, "2026-10-31 07:30", "2026-11-02 08:00"},
	} {
		got := Next(c.s, at(c.after))
		if !got.Equal(at(c.want)) {
			t.Errorf("%+v après %s : %s, attendu %s", c.s, c.after, got.Format("2006-01-02 15:04"), c.want)
		}
	}
	if !Next(config.Schedule{Type: "daily", Time: "x"}, time.Now()).IsZero() {
		t.Error("heure invalide : instant zéro attendu")
	}
}

func TestPrevAndMissed(t *testing.T) {
	loc := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = loc })
	s := config.Schedule{Type: "daily", Time: "22:30"}
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	prev := time.Date(2026, 10, 26, 22, 30, 0, 0, time.UTC)
	if got := Prev(s, now); !got.Equal(prev) {
		t.Fatalf("Prev : %v", got)
	}
	if got := Prev(s, prev); !got.Equal(prev) {
		t.Fatalf("Prev à l'heure dite : %v", got)
	}
	installed := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		last, since time.Time
		want        bool
	}{
		{time.Date(2026, 10, 25, 22, 30, 5, 0, time.UTC), installed, true},  // poste éteint lundi soir
		{time.Date(2026, 10, 26, 22, 30, 1, 0, time.UTC), installed, false}, // passage fait
		{time.Time{}, time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC), false}, // installé ce matin
		{time.Time{}, installed, true},
	} {
		if got := Missed(s, c.last, c.since, now); got != c.want {
			t.Errorf("dernier %v, depuis %v : %v", c.last, c.since, got)
		}
	}
	if Missed(config.Schedule{Type: "weekly", Time: "22:30"}, time.Time{}, installed, now) {
		t.Fatal("planification invalide : rattrapage")
	}
}
