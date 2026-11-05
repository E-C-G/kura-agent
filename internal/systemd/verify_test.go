package systemd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// systemd-analyze verify relit les unités générées comme le ferait systemd :
// directives inconnues, valeurs invalides, timer sans service.
func TestUnitsVerifiedBySystemd(t *testing.T) {
	needSystemdAnalyze(t)
	if _, err := os.Stat("/usr/bin/true"); err != nil {
		t.Skip("/usr/bin/true absent")
	}
	s := sched("hourly", "22:30", "mon", "sun")
	s.RunIfMissed = true
	s.MaxDuration.Duration = 3 * time.Hour
	for _, withDocker := range []bool{false, true} {
		u, err := Generate(s, Params{Command: "/usr/bin/true", User: "kura-agent", Docker: withDocker})
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		svc, tim := filepath.Join(dir, ServiceName), filepath.Join(dir, TimerName)
		os.WriteFile(svc, []byte(u.Service), 0o644)
		os.WriteFile(tim, []byte(u.Timer), 0o644)
		out, err := exec.Command("systemd-analyze", "verify", "--man=no", svc, tim).CombinedOutput()
		if msgs := ownMessages(string(out), dir); err != nil || len(msgs) > 0 {
			t.Fatalf("systemd-analyze verify (docker %v) : %v\n%s", withDocker, err, strings.Join(msgs, "\n"))
		}
	}
}

// ownMessages : messages de systemd-analyze, sans ceux qui portent sur les unités
// du système (/usr/lib/systemd/…), lues aussi pour résoudre les dépendances et
// signalées selon la version de systemd de la machine.
func ownMessages(out, dir string) []string {
	var msgs []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || (strings.HasPrefix(l, "/") && !strings.HasPrefix(l, dir+"/")) {
			continue
		}
		msgs = append(msgs, l)
	}
	return msgs
}

// needSystemdAnalyze : sans systemd-analyze, les tests de comparaison sont sautés,
// sauf en CI sous Linux où ils doivent tourner.
func needSystemdAnalyze(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		if os.Getenv("CI") != "" && runtime.GOOS == "linux" {
			t.Fatal("systemd-analyze absent de la CI Linux : comparaison impossible")
		}
		t.Skip("systemd-analyze absent")
	}
}
