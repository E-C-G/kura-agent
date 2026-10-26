//go:build windows

package tray

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/E-C-G/kura-agent/internal/ipc"
)

// Fenêtre de préavis : TaskDialogIndirect (comctl32 v6) avec compte à rebours.
// comctl32 v6 n'est chargé que dans un contexte d'activation qui le demande ; sans
// manifeste intégré à l'exécutable, on en crée un à partir d'un manifeste écrit
// dans le dossier temporaire de l'utilisateur.

const comctl6Manifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <dependency>
    <dependentAssembly>
      <assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0"
        processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/>
    </dependentAssembly>
  </dependency>
</assembly>`

type actCtx struct {
	Size                  uint32
	Flags                 uint32
	Source                *uint16
	ProcessorArchitecture uint16
	LangID                uint16
	AssemblyDirectory     *uint16
	ResourceName          *uint16
	ApplicationName       *uint16
	Module                uintptr
}

var (
	taskDialogOnce sync.Once
	taskDialogProc uintptr
	taskDialogErr  error
)

// loadTaskDialog active comctl32 v6 sur le thread courant et résout
// TaskDialogIndirect. Appelée une fois dans Main, sur le thread de l'interface
// (verrouillé), avant la boucle de messages : le contexte reste actif pour toute la
// vie du thread et n'est jamais désactivé.
func loadTaskDialog() (uintptr, error) {
	taskDialogOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "Kura")
		_ = os.MkdirAll(dir, 0o700)
		path := filepath.Join(dir, "comctl6.manifest")
		if err := os.WriteFile(path, []byte(comctl6Manifest), 0o600); err != nil {
			taskDialogErr = err
			return
		}
		ac := actCtx{Size: uint32(unsafe.Sizeof(actCtx{})), Source: utf16(path)}
		h, _, err := procCreateActCtxW.Call(uintptr(unsafe.Pointer(&ac)))
		if h == uintptr(windows.InvalidHandle) {
			taskDialogErr = fmt.Errorf("CreateActCtx : %v", err)
			return
		}
		var cookie uintptr
		if r, _, err := procActivateActCtx.Call(h, uintptr(unsafe.Pointer(&cookie))); r == 0 {
			taskDialogErr = fmt.Errorf("ActivateActCtx : %v", err)
			return
		}
		// Chargement par nom (et non depuis System32) : la redirection côte à côte
		// du contexte d'activation choisit comctl32 v6.
		lib, err := windows.LoadLibrary("comctl32.dll")
		if err != nil {
			taskDialogErr = err
			return
		}
		taskDialogProc, taskDialogErr = windows.GetProcAddress(lib, "TaskDialogIndirect")
	})
	return taskDialogProc, taskDialogErr
}

const (
	tdfAllowCancel   = 0x0008
	tdfCallbackTimer = 0x0800
	tdfSizeToContent = 0x01000000
	tdnCreated       = 0
	tdnButtonClicked = 2
	tdnTimer         = 4
	tdmClickButton   = wmUser + 102
	tdmSetElemText   = wmUser + 108
	tdeContent       = 0
	btnCloseNow      = 101
	btnPostpone      = 102
	tdWarningIcon    = 0xFFFF // TD_WARNING_ICON = MAKEINTRESOURCEW(-1)
)

// État du dialogue en cours (un seul à la fois : les préavis sont mis en file par
// app.prompt, et l'affichage se fait sur le thread de l'interface).
var dlg struct {
	deadline  time.Time
	shown     int
	timedOut  bool
	process   string
	seq       int     // numéro du préavis affiché
	hwnd      uintptr // fenêtre du dialogue ouvert, 0 sinon
	abandoned bool    // fermé parce que l'exécution qui l'a demandé est partie
}

// abandonDialog ferme le préavis seq s'il est encore affiché (thread de l'interface).
func abandonDialog(seq int) {
	if dlg.hwnd != 0 && dlg.seq == seq {
		dlg.abandoned = true
		procSendMessageW.Call(dlg.hwnd, tdmClickButton, btnCloseNow, 0)
	}
}

// dlgMem garde sur le tas tout ce que comctl32 lit ou écrit pendant le dialogue :
// la pile d'une goroutine peut être déplacée pendant que le dialogue est ouvert (le
// rappel exécute du Go), une adresse de pile passée au code natif deviendrait fausse.
var dlgMem struct {
	cfg     *[160]byte
	buttons []byte
	pressed *int32
	strs    []*uint16
}

var dialogCallback = windows.NewCallback(func(hwnd, msg, wParam, lParam, data uintptr) uintptr {
	switch msg {
	case tdnCreated:
		dlg.hwnd = hwnd
		procSetWindowPos.Call(hwnd, hwndTop, 0, 0, 0, 0, swpNoMove|swpNoSize)
		procSetForegroundWindow.Call(hwnd)
	case tdnTimer:
		left := int(time.Until(dlg.deadline).Round(time.Second) / time.Second)
		if left < 0 {
			left = 0
		}
		if left != dlg.shown {
			dlg.shown = left
			procSendMessageW.Call(hwnd, tdmSetElemText, tdeContent, uintptr(unsafe.Pointer(utf16(countdownText(dlg.process, left)))))
		}
		if left == 0 && !dlg.timedOut {
			dlg.timedOut = true
			procSendMessageW.Call(hwnd, tdmClickButton, btnCloseNow, 0)
		}
	}
	return 0 // S_OK : le clic ferme le dialogue
})

func displayName(process string) string {
	if strings.EqualFold(process, "outlook.exe") {
		return "Outlook"
	}
	return process
}

func countdownText(process string, left int) string {
	return fmt.Sprintf("%s sera fermé dans %d:%02d pour la sauvegarde.\nEnregistrez votre travail en cours.",
		displayName(process), left/60, left%60)
}

func human1(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
	return fmt.Sprintf("%d min", int(d/time.Minute))
}

// closeDialog affiche le préavis et renvoie le choix. À appeler sur le thread de
// l'interface (verrouillé par runtime.LockOSThread).
func closeDialog(owner uintptr, m ipc.Msg, seq int) (string, error) {
	proc, err := loadTaskDialog()
	if err != nil {
		return "", err
	}
	notice := time.Duration(m.NoticeS) * time.Second
	dlg.deadline, dlg.shown, dlg.timedOut, dlg.process = time.Now().Add(notice), m.NoticeS, false, m.Process
	dlg.seq, dlg.hwnd, dlg.abandoned = seq, 0, false
	defer func() { dlg.hwnd = 0 }()

	title := utf16("Kura – sauvegarde")
	instr := utf16(displayName(m.Process) + " va être fermé")
	content := utf16(countdownText(m.Process, m.NoticeS))
	closeTxt := utf16("Fermer maintenant")
	var postponeTxt *uint16
	buttons := make([]byte, 0, 24)
	dlgMem.strs = []*uint16{title, instr, content, closeTxt}
	addButton := func(id int32, text *uint16) {
		var b [12]byte // TASKDIALOG_BUTTON, structure compactée (pack 1)
		binary.LittleEndian.PutUint32(b[0:], uint32(id))
		binary.LittleEndian.PutUint64(b[4:], uint64(uintptr(unsafe.Pointer(text))))
		buttons = append(buttons, b[:]...)
	}
	addButton(btnCloseNow, closeTxt)
	if m.PostponesLeft > 0 && m.PostponeS > 0 {
		label := fmt.Sprintf("Reporter de %s", human1(time.Duration(m.PostponeS)*time.Second))
		if m.PostponesLeft > 1 {
			label += fmt.Sprintf(" (%d reports possibles)", m.PostponesLeft)
		} else {
			label += " (dernier report)"
		}
		postponeTxt = utf16(label)
		dlgMem.strs = append(dlgMem.strs, postponeTxt)
		addButton(btnPostpone, postponeTxt)
	}
	dlgMem.buttons = buttons

	// TASKDIALOGCONFIG est déclarée avec #pragma pack(1) : on la construit octet par
	// octet (160 octets en 64 bits) plutôt qu'avec une structure Go alignée.
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return "", errors.New("fenêtre de préavis : Windows 64 bits uniquement")
	}
	dlgMem.cfg = new([160]byte)
	dlgMem.pressed = new(int32)
	cfg := dlgMem.cfg
	put32 := func(off int, v uint32) { binary.LittleEndian.PutUint32(cfg[off:], v) }
	putp := func(off int, v uintptr) { binary.LittleEndian.PutUint64(cfg[off:], uint64(v)) }
	put32(0, uint32(len(cfg)))
	putp(4, owner)
	put32(20, tdfCallbackTimer|tdfSizeToContent)
	putp(28, uintptr(unsafe.Pointer(title)))
	putp(36, tdWarningIcon)
	putp(44, uintptr(unsafe.Pointer(instr)))
	putp(52, uintptr(unsafe.Pointer(content)))
	put32(60, uint32(len(buttons)/12))
	putp(64, uintptr(unsafe.Pointer(&dlgMem.buttons[0])))
	put32(72, btnCloseNow)
	putp(140, dialogCallback)

	hr, _, _ := syscallN(proc, uintptr(unsafe.Pointer(&dlgMem.cfg[0])), uintptr(unsafe.Pointer(dlgMem.pressed)), 0, 0)
	pressed := *dlgMem.pressed
	dlgMem.cfg, dlgMem.buttons, dlgMem.pressed, dlgMem.strs = nil, nil, nil, nil
	if hr != 0 {
		return "", fmt.Errorf("TaskDialogIndirect : 0x%x", uint32(hr))
	}
	switch {
	case dlg.abandoned:
		return "", nil
	case pressed == btnPostpone:
		return ipc.ChoicePostpone, nil
	case dlg.timedOut:
		return ipc.ChoiceTimeout, nil
	default:
		return ipc.ChoiceCloseNow, nil
	}
}
