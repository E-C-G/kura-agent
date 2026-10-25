//go:build !windows

package execx

import (
	"os/exec"
	"syscall"
)

// Hors Windows : groupe de processus dédié, arrêté en entier à l'annulation.
func platformSetup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

func killChildrenOnExit() error { return nil }
