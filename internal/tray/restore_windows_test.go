//go:build windows

package tray

import (
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/E-C-G/kura-agent/internal/ipc"
)

// La fenêtre se construit, se remplit et rend ce qui est coché (sans canal ni tâche).
func TestRestoreWindowControls(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, err := loadTaskDialog(); err != nil {
		t.Skip("comctl32 v6 indisponible :", err)
	}
	w, err := newRestoreWin(nil)
	if err != nil {
		t.Skip("pas de fenêtre possible ici :", err)
	}
	defer procDestroyWindow.Call(w.hwnd)
	for _, id := range []int{idFolder, idBrowse, idDate, idShow, idList, idHistory, idModeCopy, idModeMissing, idRestore, idProgress} {
		if w.ctl[id] == 0 {
			t.Fatalf("contrôle %d absent", id)
		}
	}
	now := time.Now()
	files := []ipc.RestoreFile{
		{Rel: "Factures/2026.pdf", Size: 1000, BackedUp: now},
		{Rel: "Notes.txt", Size: 10, BackedUp: now, DeletedSince: &now},
		{Rel: "../x", Size: 1, BackedUp: now, Problem: "segment « .. » refusé"},
	}
	w.populate(files)
	if n, _, _ := procSendMessageW.Call(w.ctl[idList], lvmGetItemCount, 0, 0); n != 3 {
		t.Fatalf("%d lignes", n)
	}
	w.setAll(true)
	if c := w.checked(); len(c) != 2 || c[0] != 0 || c[1] != 1 {
		t.Fatalf("cochés : %v", c)
	}
	w.setChecked(0, false)
	if c := w.checked(); len(c) != 1 || c[0] != 1 {
		t.Fatalf("après décochage : %v", c)
	}
	if w.chosenDate() != nil {
		t.Fatal("aujourd'hui : dernière sauvegarde attendue")
	}
	st := sysTime(now.AddDate(0, 0, -10))
	procSendMessageW.Call(w.ctl[idDate], dtmSetSystemTime, gdtValid, uintptr(unsafe.Pointer(&st)))
	if at := w.chosenDate(); at == nil || at.Day() != now.AddDate(0, 0, -10).Day() || at.Hour() != 23 {
		t.Fatalf("date choisie : %v", at)
	}
	if w.mode() != ipc.RestoreCopy {
		t.Fatal("mode par défaut")
	}
	procSendMessageW.Call(w.ctl[idModeCopy], bmSetCheck, 0, 0)
	procSendMessageW.Call(w.ctl[idModeMissing], bmSetCheck, 1, 0)
	if w.mode() != ipc.RestoreMissing {
		t.Fatal("mode à leur place")
	}
	w.setRoots([]string{`C:\Users\alice\Documents`, `D:\Projets`}, `D:\Projets`)
	if i, _, _ := procSendMessageW.Call(w.ctl[idFolder], cbGetCurSel, 0, 0); i != 1 {
		t.Fatalf("raccourci choisi : %d", i)
	}
	w.showVersions([]ipc.RestoreVersion{{Date: now, Size: 3, Current: true}, {Date: now.AddDate(0, 0, -1), Deleted: true}})
	if n, _, _ := procSendMessageW.Call(w.ctl[idHistory], lvmGetItemCount, 0, 0); n != 2 {
		t.Fatalf("historique : %d lignes", n)
	}
	procDestroyWindow.Call(w.hwnd)
	if rw != nil {
		t.Fatal("fenêtre non libérée")
	}
	_ = windows.Handle(0)
}
