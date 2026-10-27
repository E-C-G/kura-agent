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
		{"windows", "", `E:\PD`, layout{filepath.Join(`E:\PD`, "Kura"), filepath.Join(`E:\PD`, "Kura"), filepath.Join(`E:\PD`, "Kura", "logs"), filepath.Join(`E:\PD`, "Kura", "credentials")}},
		{"windows", "", "", layout{filepath.Join(`C:\ProgramData`, "Kura"), filepath.Join(`C:\ProgramData`, "Kura"), filepath.Join(`C:\ProgramData`, "Kura", "logs"), filepath.Join(`C:\ProgramData`, "Kura", "credentials")}},
		{"linux", "", "", layout{"/var/lib/kura", "/etc/kura", "/var/log/kura", "/etc/credstore.encrypted"}},
		{"darwin", "", "", layout{"/Library/Application Support/Kura", "/Library/Application Support/Kura", "/Library/Logs/Kura", "/Library/Application Support/Kura/credentials"}},
		{"linux", "/tmp/k", "", layout{"/tmp/k", "/tmp/k", filepath.Join("/tmp/k", "logs"), filepath.Join("/tmp/k", "credentials")}},
	} {
		if got := layoutFor(c.goos, c.override, c.programData); got != c.want {
			t.Errorf("%s %q : %+v, attendu %+v", c.goos, c.override, got, c.want)
		}
	}
}
