//go:build windows

package tray

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ownProcesses renvoie les PID des processus de ce nom dans la session de l'icône.
func ownProcesses(name string) map[uint32]bool {
	var mySession uint32
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &mySession)
	out := map[uint32]bool{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if !strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), name) {
			continue
		}
		var s uint32
		if windows.ProcessIdToSessionId(e.ProcessID, &s) == nil && s == mySession {
			out[e.ProcessID] = true
		}
	}
	return out
}

var (
	enumMu     sync.Mutex
	enumPIDs   map[uint32]bool
	enumWins   []uintptr
	enumHidden []uintptr // fenêtres principales masquées (Outlook « masquer une fois réduit »)
	enumCB     = windows.NewCallback(func(hwnd, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if enumPIDs[pid] {
			visible, _, _ := procIsWindowVisible.Call(hwnd)
			owner, _, _ := procGetWindow.Call(hwnd, gwOwner)
			switch {
			case owner != 0:
			case visible != 0:
				enumWins = append(enumWins, hwnd)
			case windowClass(hwnd) == "rctrl_renwnd32": // fenêtre principale d'Outlook
				enumHidden = append(enumHidden, hwnd)
			}
		}
		return 1 // continuer
	})
)

func windowClass(hwnd uintptr) string {
	var buf [64]uint16
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

// closeApp ferme en douceur les processus de ce nom dans la session : WM_CLOSE à leurs
// fenêtres principales, comme un clic sur la croix, puis attente de leur sortie au plus
// graceful. Renvoie le nombre d'instances encore ouvertes.
func closeApp(name string, graceful time.Duration) int {
	pids := ownProcesses(name)
	if len(pids) == 0 {
		return 0
	}
	enumMu.Lock()
	enumPIDs, enumWins, enumHidden = pids, nil, nil
	procEnumWindows.Call(enumCB, 0)
	wins := enumWins
	if len(wins) == 0 {
		wins = enumHidden
	}
	enumPIDs, enumWins, enumHidden = nil, nil, nil
	enumMu.Unlock()
	for _, w := range wins {
		procPostMessageW.Call(w, wmClose, 0, 0)
	}
	deadline := time.Now().Add(graceful)
	for {
		left := ownProcesses(name)
		if len(left) == 0 || time.Now().After(deadline) {
			return len(left)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// startApp relance un processus dans la session de l'icône. Le chemin vient de run
// (exécutable fermé) ; à défaut, du registre App Paths. On ne lance qu'un exécutable
// portant le nom attendu, et rien s'il tourne déjà.
func startApp(name, path string) error {
	if len(ownProcesses(name)) > 0 {
		return nil
	}
	if path == "" {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`+name, registry.QUERY_VALUE)
		if err == nil {
			path, _, _ = k.GetStringValue("")
			k.Close()
		}
	}
	if path == "" {
		return fmt.Errorf("emplacement de %s inconnu", name)
	}
	if !strings.EqualFold(filepath.Base(path), name) {
		return fmt.Errorf("%s ne correspond pas à %s : relance refusée", path, name)
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	cmd := exec.Command(path)
	cmd.Dir = filepath.Dir(path)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
