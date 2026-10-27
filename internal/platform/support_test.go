package platform

import (
	"runtime"
	"testing"
)

// Le tableau de prise en charge suit le système de compilation : tout sous Windows,
// rien ailleurs (tant qu'aucune fonction n'y est écrite).
func TestSupportTable(t *testing.T) {
	for _, f := range All {
		if got, want := Supported(f), runtime.GOOS == "windows"; got != want {
			t.Errorf("%s : pris en charge = %v sous %s", f, got, runtime.GOOS)
		}
	}
}
