package systemd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// systemd-analyze verify relit les unités générées comme le ferait systemd :
// directives inconnues, valeurs invalides, timer sans service.
func TestUnitsVerifiedBySystemd(t *testing.T) {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze absent")
	}
	if _, err := os.Stat("/usr/bin/true"); err != nil {
		t.Skip("/usr/bin/true absent")
	}
	s := sched("hourly", "22:30", "mon", "sun")
	s.RunIfMissed = true
	s.MaxDuration.Duration = 3 * time.Hour
	u, err := Generate(s, Params{Command: "/usr/bin/true", User: "kura-agent"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	svc, tim := filepath.Join(dir, ServiceName), filepath.Join(dir, TimerName)
	os.WriteFile(svc, []byte(u.Service), 0o644)
	os.WriteFile(tim, []byte(u.Timer), 0o644)
	out, err := exec.Command("systemd-analyze", "verify", "--man=no", svc, tim).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("systemd-analyze verify : %v\n%s", err, out)
	}
}
