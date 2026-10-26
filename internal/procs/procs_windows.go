//go:build windows

package procs

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	debugOnce sync.Once
	debugErr  error
)

// enableDebugPrivilege active SeDebugPrivilege : sans lui, même un administrateur ne peut
// pas ouvrir les processus d'un autre utilisateur (c'est ce que fait taskkill). Activé avant
// toute recherche, pour que la lecture du numéro de session réussisse aussi.
func enableDebugPrivilege() error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("jeton du processus : %w", err)
	}
	defer tok.Close()
	var luid windows.LUID
	name, _ := windows.UTF16PtrFromString("SeDebugPrivilege")
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return fmt.Errorf("SeDebugPrivilege : %w", err)
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	// AdjustTokenPrivileges « réussit » même si le privilège n'a pas été accordé :
	// seul GetLastError le dit, d'où le thread verrouillé entre les deux appels.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.AdjustTokenPrivileges(tok, false, &tp, uint32(unsafe.Sizeof(tp)), nil, nil); err != nil {
		return fmt.Errorf("activation de SeDebugPrivilege : %w", err)
	}
	if errors.Is(windows.GetLastError(), windows.ERROR_NOT_ALL_ASSIGNED) {
		return errors.New("SeDebugPrivilege non accordé : lancer la tâche avec les autorisations maximales")
	}
	return nil
}

func debugPrivilege() error {
	debugOnce.Do(func() { debugErr = enableDebugPrivilege() })
	return debugErr
}

func (system) Find(name string) ([]Process, error) {
	_ = debugPrivilege() // l'échec éventuel est signalé par Terminate
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
		out = append(out, Process{PID: int(e.ProcessID), Name: exe, Session: int(sid), Path: imagePath(e.ProcessID)})
	}
	return out, nil
}

// imagePath renvoie le chemin complet de l'exécutable d'un processus, ou "".
func imagePath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &size) != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

// Terminate arrête p après avoir vérifié, sur le processus ouvert lui-même, qu'il porte
// toujours le nom attendu : un PID peut avoir été réattribué entre-temps.
func (system) Terminate(p Process) error {
	privErr := debugPrivilege()
	h, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(p.PID))
	if err != nil {
		if privErr != nil {
			return fmt.Errorf("ouverture du processus %d : %w (%v)", p.PID, err, privErr)
		}
		return fmt.Errorf("ouverture du processus %d : %w", p.PID, err)
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return fmt.Errorf("nom du processus %d illisible, arrêt annulé : %w", p.PID, err)
	}
	if got := filepath.Base(windows.UTF16ToString(buf[:size])); !strings.EqualFold(got, p.Name) {
		return fmt.Errorf("le PID %d désigne maintenant %q et non %q : arrêt annulé", p.PID, got, p.Name)
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("arrêt du processus %d : %w", p.PID, err)
	}
	if ev, _ := windows.WaitForSingleObject(h, 10_000); ev != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("le processus %d ne s'est pas arrêté dans les 10 s", p.PID)
	}
	return nil
}

// EnableDebugPrivilege active SeDebugPrivilege pour tout le processus : nécessaire
// aussi pour inspecter le processus d'une icône d'une autre session (ipc.Dial).
func EnableDebugPrivilege() error { return debugPrivilege() }
