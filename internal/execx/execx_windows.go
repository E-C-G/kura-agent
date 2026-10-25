//go:build windows

package execx

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformSetup : pas de fenêtre console. À l'annulation, exec tue aws.exe ;
// ses éventuels enfants meurent avec l'agent grâce au Job Object (killChildrenOnExit).
func platformSetup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}

// killChildrenOnExit place l'agent dans un Job Object « tuer à la fermeture ».
// Les processus enfants (aws.exe et ce qu'il lance) héritent du Job : si l'agent
// s'arrête, de quelque façon que ce soit, Windows les termine tous.
// Le handle n'est jamais fermé volontairement : il se ferme à la sortie du processus.
func killChildrenOnExit() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject : %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("SetInformationJobObject : %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("AssignProcessToJobObject : %w", err)
	}
	return nil
}
