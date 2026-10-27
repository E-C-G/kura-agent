package launchd

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/schedtask"
	"github.com/E-C-G/kura-agent/pkg/config"
)

func sched(typ, at string, days ...string) config.Schedule {
	return config.Schedule{Type: typ, Time: at, Days: days}
}

var params = Params{Command: "/usr/local/bin/kura-agent", User: "_kura",
	DataDir: "/Library/Application Support/Kura", LogsDir: "/Library/Logs/Kura"}

func TestIntervals(t *testing.T) {
	all := -1
	for _, c := range []struct {
		s    config.Schedule
		want []Interval
	}{
		{sched("daily", "22:30"), []Interval{{all, 22, 30}}},
		{sched("daily", "22:30", "fri", "mon", "fri"), []Interval{{1, 22, 30}, {5, 22, 30}}},
		{sched("weekly", "07:05", "sun"), []Interval{{0, 7, 5}}},
		{sched("hourly", "08:15"), []Interval{{all, all, 15}}},
		{sched("hourly", ""), []Interval{{all, all, 0}}},
		{sched("hourly", "00:10", "sat"), []Interval{{6, all, 10}}},
		{sched("hourly", "22:30", "mon"), []Interval{{1, 22, 30}, {1, 23, 30},
			{2, 0, 30}, {2, 1, 30}, {2, 2, 30}, {2, 3, 30}, {2, 4, 30}, {2, 5, 30}, {2, 6, 30}, {2, 7, 30}, {2, 8, 30}, {2, 9, 30},
			{2, 10, 30}, {2, 11, 30}, {2, 12, 30}, {2, 13, 30}, {2, 14, 30}, {2, 15, 30}, {2, 16, 30}, {2, 17, 30}, {2, 18, 30},
			{2, 19, 30}, {2, 20, 30}, {2, 21, 30}}},
		{sched("hourly", "09:00", "mon", "tue", "wed", "thu", "fri", "sat", "sun"), []Interval{{all, all, 0}}},
	} {
		got, err := Intervals(c.s)
		if err != nil || len(got) != len(c.want) {
			t.Errorf("%+v : %v %v", c.s, got, err)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%+v : %v, attendu %v", c.s, got, c.want)
				break
			}
		}
	}
	for _, s := range []config.Schedule{sched("weekly", "22:30"), sched("daily", "25:00"), sched("monthly", "22:30"), sched("daily", "22:30", "lun")} {
		if _, err := Intervals(s); err == nil {
			t.Errorf("%+v accepté", s)
		}
	}
}

// Les entrées générées, lues comme launchd les lit, donnent les mêmes passages que
// la tâche Windows (schedtask.Next).
func TestSameRunsAsWindows(t *testing.T) {
	loc := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = loc })
	cases := []config.Schedule{
		sched("daily", "22:30"), sched("daily", "22:30", "mon", "fri"), sched("weekly", "07:05", "sun"),
		sched("weekly", "00:00", "sat", "sun"), sched("hourly", "08:15"), sched("hourly", "22:30", "mon", "sun"),
		sched("hourly", "00:10", "sat"), sched("hourly", "13:45", "wed"), sched("hourly", "23:59", "tue", "wed", "thu"),
	}
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) // un lundi
	for _, s := range cases {
		ivs, err := Intervals(s)
		if err != nil {
			t.Fatal(err)
		}
		a, b := base, base
		for i := 0; i < 200; i++ {
			a, b = Next(ivs, a), schedtask.Next(s, b)
			if a.IsZero() || !a.Equal(b) {
				t.Fatalf("%+v, passage %d : launchd %v, Windows %v", s, i, a, b)
			}
		}
	}
}

func TestGenerate(t *testing.T) {
	s := sched("daily", "22:30", "mon", "fri")
	s.RunIfMissed = true
	p, err := Generate(s, params)
	if err != nil {
		t.Fatal(err)
	}
	sync, catch := string(p.Sync), string(p.CatchUp)
	for _, want := range []string{
		"<string>" + Label + "</string>", "<string>/usr/local/bin/kura-agent</string>\n\t\t<string>run</string>\n\t\t<string>--scheduled</string>\n\t</array>",
		"<key>UserName</key>\n\t<string>_kura</string>", "<key>Weekday</key>\n\t\t\t<integer>5</integer>",
		"<string>/Library/Application Support/Kura</string>", "launchd-sync.log", "<key>LowPriorityIO</key>",
	} {
		if !strings.Contains(sync, want) {
			t.Errorf("démon de sauvegarde sans %q :\n%s", want, sync)
		}
	}
	if strings.Contains(sync, "RunAtLoad") || !strings.Contains(catch, "<key>RunAtLoad</key>\n\t<true/>") ||
		!strings.Contains(catch, "<string>--catch-up</string>") || strings.Contains(catch, "StartCalendarInterval") {
		t.Fatalf("rattrapage :\n%s", catch)
	}
	for _, data := range [][]byte{p.Sync, p.CatchUp} {
		wellFormed(t, data)
		lint(t, data)
	}
	// Sans run_if_missed : pas de démon de rattrapage.
	s.RunIfMissed = false
	if p, err := Generate(s, params); err != nil || p.CatchUp != nil {
		t.Fatalf("%v %q", err, p.CatchUp)
	}
	// Caractères spéciaux échappés.
	odd := params
	odd.DataDir = "/Volumes/A&B <x>"
	if p, err := Generate(s, odd); err != nil || !strings.Contains(string(p.Sync), "A&amp;B &lt;x&gt;") {
		t.Fatalf("%v", err)
	} else {
		wellFormed(t, p.Sync)
	}
	for _, bad := range []Params{
		{Command: "kura-agent", User: "_kura", DataDir: "/d", LogsDir: "/l"},
		{Command: "/k", User: "", DataDir: "/d", LogsDir: "/l"},
		{Command: "/k", User: "a b", DataDir: "/d", LogsDir: "/l"},
		{Command: "/k", User: "_kura", DataDir: "d", LogsDir: "/l"},
	} {
		if _, err := Generate(s, bad); err == nil {
			t.Errorf("%+v accepté", bad)
		}
	}
}

func wellFormed(t *testing.T, data []byte) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(string(data)))
	d.Strict = true
	for {
		if _, err := d.Token(); errors.Is(err, io.EOF) {
			return
		} else if err != nil {
			t.Fatalf("XML invalide : %v\n%s", err, data)
		}
	}
}

// lint : plutil (macOS) relit la plist ; obligatoire dans la CI macOS.
func lint(t *testing.T, data []byte) {
	t.Helper()
	if _, err := exec.LookPath("plutil"); err != nil {
		if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
			t.Fatal("plutil absent de la CI macOS")
		}
		return
	}
	f := filepath.Join(t.TempDir(), "x.plist")
	if err := os.WriteFile(f, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
		t.Fatalf("plutil : %v %s\n%s", err, out, data)
	}
}

func TestManager(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	m := Manager{Dir: dir, Run: func(a ...string) error { calls = append(calls, strings.Join(a, " ")); return nil }}
	sp := filepath.Join(dir, PlistName(Label))
	cp := filepath.Join(dir, PlistName(CatchUpLabel))
	if err := m.Install(Plists{Sync: []byte("s1"), CatchUp: []byte("c1")}); err != nil || !m.Exists() || m.InstalledAt().IsZero() {
		t.Fatal(err)
	}
	want := "bootout system/" + Label + "; enable system/" + Label + "; bootstrap system " + sp +
		"; bootout system/" + CatchUpLabel + "; enable system/" + CatchUpLabel + "; bootstrap system " + cp
	if got := strings.Join(calls, "; "); got != want {
		t.Fatalf("appels : %s", got)
	}
	if st, _ := os.Stat(sp); runtime.GOOS != "windows" && st.Mode().Perm() != 0o644 {
		t.Fatalf("droits : %v", st.Mode())
	}
	// run_if_missed retiré : le rattrapage disparaît.
	calls = nil
	if err := m.Install(Plists{Sync: []byte("s2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cp); !errors.Is(err, os.ErrNotExist) || !strings.Contains(strings.Join(calls, ";"), "bootout system/"+CatchUpLabel) {
		t.Fatalf("rattrapage resté : %v %v", err, calls)
	}
	calls = nil
	if err := m.Remove(); err != nil || m.Exists() || !m.InstalledAt().IsZero() {
		t.Fatal(err)
	}
	if got := strings.Join(calls, "; "); got != "bootout system/"+Label {
		t.Fatalf("appels : %s", got)
	}
	if err := m.Remove(); err != nil {
		t.Fatal("deuxième retrait :", err)
	}
	failing := Manager{Dir: t.TempDir(), Run: func(a ...string) error {
		if a[0] == "bootstrap" {
			return errors.New("refus")
		}
		return nil
	}}
	if err := failing.Install(Plists{Sync: []byte("s")}); err == nil {
		t.Fatal("échec de launchctl ignoré")
	}
}

func TestSystemOutsideMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS")
	}
	if err := System().Install(Plists{Sync: []byte("s")}); err == nil {
		t.Fatal("launchd hors macOS")
	}
}
