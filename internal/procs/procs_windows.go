//go:build windows

package procs

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var debugOnce sync.Once

// enableDebugPrivilege active SeDebugPrivilege : sans lui, même un administrateur
// ne peut pas ouvrir les processus d'un autre utilisateur (c'est ce que fait taskkill).
func enableDebugPrivilege() {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return
	}
	defer tok.Close()
	var luid windows.LUID
	name, _ := windows.UTF16PtrFromString("SeDebugPrivilege")
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	_ = windows.AdjustTokenPrivileges(tok, false, &tp, uint32(unsafe.Sizeof(tp)), nil, nil)
}

func (system) Find(name string) ([]Process, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("liste des processus : %w", err)
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []Process
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		exe := windows.UTF16ToString(e.ExeFile[:])
		if !strings.EqualFold(exe, name) {
			continue
		}
		var sid uint32
		_ = windows.ProcessIdToSessionId(e.ProcessID, &sid)
		out = append(out, Process{PID: int(e.ProcessID), Name: exe, Session: int(sid)})
	}
	return out, nil
}

func (system) Terminate(p Process) error {
	debugOnce.Do(enableDebugPrivilege)
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(p.PID))
	if err != nil {
		return fmt.Errorf("ouverture du processus %d : %w", p.PID, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("arrêt du processus %d : %w", p.PID, err)
	}
	if ev, _ := windows.WaitForSingleObject(h, 10_000); ev != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("le processus %d ne s'est pas arrêté dans les 10 s", p.PID)
	}
	return nil
}
