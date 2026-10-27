package systemd

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/schedtask"
	"github.com/E-C-G/kura-agent/pkg/config"
)

func sched(typ, at string, days ...string) config.Schedule {
	return config.Schedule{Type: typ, Time: at, Days: days}
}

func TestOnCalendar(t *testing.T) {
	for _, c := range []struct {
		s    config.Schedule
		want []string
	}{
		{sched("daily", "22:30"), []string{"*-*-* 22:30:00"}},
		{sched("daily", "22:30", "fri", "mon"), []string{"Mon,Fri *-*-* 22:30:00"}},
		{sched("weekly", "07:05", "sun"), []string{"Sun *-*-* 07:05:00"}},
		{sched("hourly", "08:15"), []string{"*-*-* *:15:00"}},
		{sched("hourly", ""), []string{"*-*-* *:00:00"}},
		{sched("hourly", "00:10", "sat", "sun"), []string{"Sat,Sun *-*-* *:10:00"}},
		{sched("hourly", "22:30", "mon", "sun"), []string{"Mon,Sun *-*-* 22..23:30:00", "Mon,Tue *-*-* 00..21:30:00"}},
		{sched("hourly", "09:00", "mon", "tue", "wed", "thu", "fri", "sat", "sun"), []string{"*-*-* *:00:00"}},
	} {
		got, err := OnCalendar(c.s)
		if err != nil || strings.Join(got, " | ") != strings.Join(c.want, " | ") {
			t.Errorf("%+v : %q %v, attendu %q", c.s, got, err, c.want)
		}
	}
	for _, s := range []config.Schedule{sched("weekly", "22:30"), sched("daily", "25:00"), sched("monthly", "22:30"), sched("daily", "22:30", "lun")} {
		if _, err := OnCalendar(s); err == nil {
			t.Errorf("%+v accepté", s)
		}
	}
}

func TestGenerate(t *testing.T) {
	s := sched("daily", "22:30", "mon", "fri")
	s.RunIfMissed = true
	s.MaxDuration.Duration = 2 * time.Hour
	u, err := Generate(s, Params{Command: "/usr/bin/kura-agent", User: "kura-agent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"ExecStart=/usr/bin/kura-agent run --quiet --scheduled", "User=kura-agent", "TimeoutStartSec=8100",
		"AmbientCapabilities=CAP_DAC_READ_SEARCH", "ProtectSystem=strict", "StateDirectory=kura", "SuccessExitStatus=1"} {
		if !strings.Contains(u.Service, w) {
			t.Errorf("service sans %q", w)
		}
	}
	for _, w := range []string{"Unit=kura-sync.service", "OnCalendar=Mon,Fri *-*-* 22:30:00", "Persistent=true", "WantedBy=timers.target"} {
		if !strings.Contains(u.Timer, w) {
			t.Errorf("timer sans %q", w)
		}
	}
	for _, p := range []Params{{Command: "kura-agent", User: "kura-agent"}, {Command: "/usr/bin/kura agent", User: "kura-agent"},
		{Command: "/usr/bin/kura-agent", User: ""}, {Command: "/usr/bin/kura-agent", User: "root\nExecStartPre=/bin/sh"}} {
		if _, err := Generate(s, p); err == nil {
			t.Errorf("%+v accepté", p)
		}
	}
}

func TestManager(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	m := Manager{Dir: dir, Run: func(a ...string) error { calls = append(calls, strings.Join(a, " ")); return nil }}
	u := Units{Service: "s1\n", Timer: "t1\n"}
	if err := m.Install(u); err != nil || !m.Exists() {
		t.Fatal(err)
	}
	if got := strings.Join(calls, "; "); got != "daemon-reload; enable kura-sync.timer; restart kura-sync.timer" {
		t.Fatalf("appels : %s", got)
	}
	// Rien de changé : pas de rechargement.
	calls = nil
	if err := m.Install(u); err != nil || strings.Contains(strings.Join(calls, ";"), "daemon-reload") {
		t.Fatalf("%v %v", err, calls)
	}
	calls = nil
	if err := m.Remove(); err != nil || m.Exists() {
		t.Fatal(err)
	}
	if got := strings.Join(calls, "; "); got != "disable --now kura-sync.timer; daemon-reload" {
		t.Fatalf("appels : %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ServiceName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("service resté en place")
	}
	failing := Manager{Dir: t.TempDir(), Run: func(a ...string) error { return errors.New("refus") }}
	if err := failing.Install(u); err == nil {
		t.Fatal("échec de systemctl ignoré")
	}
}

// Les expressions générées, lues par systemd lui-même (systemd-analyze calendar),
// donnent les mêmes déclenchements que la tâche Windows (schedtask.Next).
func TestSameRunsAsWindows(t *testing.T) {
	needSystemdAnalyze(t)
	// systemd-analyze (TZ) et schedtask.Next (time.Local) dans le même fuseau.
	t.Setenv("TZ", "UTC")
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })
	cases := []config.Schedule{
		sched("daily", "22:30"), sched("daily", "22:30", "mon", "fri"), sched("weekly", "07:05", "sun"),
		sched("hourly", "08:15"), sched("hourly", "22:30", "mon", "sun"), sched("hourly", "00:10", "sat"),
		sched("hourly", "13:45", "wed"),
	}
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) // un lundi
	for _, s := range cases {
		exprs, err := OnCalendar(s)
		if err != nil {
			t.Fatal(err)
		}
		const n = 60
		var got []time.Time
		for _, e := range exprs {
			out, err := exec.Command("systemd-analyze", "calendar", "--base-time="+base.Format("2006-01-02 15:04:05 UTC"),
				"--iterations="+itoa(n), e).CombinedOutput()
			if err != nil {
				t.Fatalf("%s : %v %s", e, err, out)
			}
			got = append(got, parseElapses(t, string(out))...)
		}
		sort.Slice(got, func(i, j int) bool { return got[i].Before(got[j]) })
		if len(got) < n {
			t.Fatalf("%+v : %d passages lus dans la sortie de systemd-analyze", s, len(got))
		}
		got = got[:n]
		prev := base
		for i := 0; i < n; i++ {
			want := schedtask.Next(s, prev).UTC()
			if !got[i].Equal(want) {
				t.Errorf("%+v, passage %d : systemd %s, Windows %s", s, i+1, got[i], want)
				break
			}
			prev = want
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// parseElapses lit « Next elapse: Mon 2026-10-26 22:30:00 UTC » et « Iteration #2: … »
// (« Iter. #2: » selon les versions).
func parseElapses(t *testing.T, out string) []time.Time {
	var ts []time.Time
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(l, "Next elapse:") && !strings.HasPrefix(l, "Iter") {
			continue
		}
		f := strings.Fields(l[strings.Index(l, ":")+1:])
		if len(f) < 3 {
			continue
		}
		tm, err := time.Parse("2006-01-02 15:04:05", f[1]+" "+f[2])
		if err != nil {
			t.Fatalf("%q : %v", l, err)
		}
		ts = append(ts, tm.UTC())
	}
	return ts
}
