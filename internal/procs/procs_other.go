//go:build !windows

package procs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Hors Windows : remplaçant de développement, lecture de /proc (Linux seulement).
// Les commandes refusent les pré-tâches sur ces systèmes sauf KURA_DEV=1
// (platform.AppControl).
func (system) Find(name string) ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(cmd) == 0 {
			continue
		}
		exe := filepath.Base(strings.SplitN(string(cmd), "\x00", 2)[0])
		if strings.EqualFold(exe, name) {
			out = append(out, Process{PID: pid, Name: exe})
		}
	}
	return out, nil
}

func (system) Terminate(p Process) error { return syscall.Kill(p.PID, syscall.SIGKILL) }

// EnableDebugPrivilege : sans objet hors Windows.
func EnableDebugPrivilege() error { return nil }
