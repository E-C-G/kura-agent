package paths

import (
	"path/filepath"
	"testing"
)

func TestLayouts(t *testing.T) {
	for _, c := range []struct {
		goos, override, programData string
		want                        layout
	}{
		{"windows", "", `E:\PD`, layout{filepath.Join(`E:\PD`, "Kura"), filepath.Join(`E:\PD`, "Kura"), filepath.Join(`E:\PD`, "Kura", "logs")}},
		{"windows", "", "", layout{filepath.Join(`C:\ProgramData`, "Kura"), filepath.Join(`C:\ProgramData`, "Kura"), filepath.Join(`C:\ProgramData`, "Kura", "logs")}},
		{"linux", "", "", layout{"/var/lib/kura", "/etc/kura", "/var/log/kura"}},
		{"darwin", "", "", layout{"/Library/Application Support/Kura", "/Library/Application Support/Kura", "/Library/Logs/Kura"}},
		{"linux", "/tmp/k", "", layout{"/tmp/k", "/tmp/k", filepath.Join("/tmp/k", "logs")}},
	} {
		if got := layoutFor(c.goos, c.override, c.programData); got != c.want {
			t.Errorf("%s %q : %+v, attendu %+v", c.goos, c.override, got, c.want)
		}
	}
}
