package launchd

import (
	"fmt"
	"os/exec"
	"strings"
)

// System : gestionnaire réel (launchctl).
func System() Manager { return Manager{Run: launchctl} }

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s : %v %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
