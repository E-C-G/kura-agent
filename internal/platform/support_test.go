package platform

import (
	"runtime"
	"testing"
)

// Le tableau de prise en charge suit le système de compilation.
func TestSupportTable(t *testing.T) {
	want := map[string]map[Feature]bool{
		"windows": {Scheduling: true, Vault: true, Install: true, AWSCLISetup: true, Tray: true, AppControl: true},
		"linux":   {Install: true, Scheduling: true, Vault: true, AWSCLISetup: true},
	}[runtime.GOOS]
	for _, f := range All {
		if got := Supported(f); got != want[f] {
			t.Errorf("%s : pris en charge = %v sous %s", f, got, runtime.GOOS)
		}
	}
}
