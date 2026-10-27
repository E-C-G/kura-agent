package systemd

import (
	"fmt"
	"os/exec"
	"strings"
)

// System : gestionnaire réel (systemctl).
func System() Manager { return Manager{Run: systemctl} }

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s : %v %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
