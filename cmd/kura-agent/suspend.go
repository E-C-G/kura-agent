package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/state"
)

// suspendUntil interprète l'argument de suspend : une durée (1h, 90m), « tomorrow »
// (demain 08:00) ; ok=false pour « off » (reprise).
func suspendUntil(arg string, now time.Time) (until time.Time, ok bool, err error) {
	switch strings.ToLower(arg) {
	case "off", "resume", "reprendre":
		return time.Time{}, false, nil
	case "tomorrow", "demain":
		d := now.AddDate(0, 0, 1)
		return time.Date(d.Year(), d.Month(), d.Day(), 8, 0, 0, 0, now.Location()), true, nil
	}
	d, err := time.ParseDuration(arg)
	if err != nil || d <= 0 || d > 31*24*time.Hour {
		return time.Time{}, false, errors.New("durée attendue (ex. 1h, 12h), tomorrow ou off ; 31 jours au plus")
	}
	return now.Add(d), true, nil
}

func cmdSuspend(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage : kura-agent suspend DURÉE | tomorrow | off")
		return 2
	}
	now := time.Now()
	until, on, err := suspendUntil(args[0], now)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if !on {
		if err := state.ClearSuspension(paths.SuspendFile()); err != nil {
			fmt.Fprintln(os.Stderr, "reprise impossible (console d'administration ?) :", err)
			return 2
		}
		fmt.Println("planification reprise")
		return 0
	}
	by := ""
	if u, err := user.Current(); err == nil {
		by = u.Username
	}
	if err := state.WriteSuspension(paths.SuspendFile(), state.Suspension{Until: until, By: by, At: now}); err != nil {
		fmt.Fprintln(os.Stderr, "suspension impossible (console d'administration ?) :", err)
		return 2
	}
	fmt.Printf("planification suspendue jusqu'au %s\n", until.Format("02/01/2006 15:04"))
	return 0
}
