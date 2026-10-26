//go:build windows

package tray

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Appels Win32 de l'icône. Tout passe par golang.org/x/sys/windows et des LazyProc :
// pas de dépendance graphique externe.

var (
	user32                        = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW          = user32.NewProc("RegisterClassExW")
	procCreateWindowExW           = user32.NewProc("CreateWindowExW")
	procDefWindowProcW            = user32.NewProc("DefWindowProcW")
	procDestroyWindow             = user32.NewProc("DestroyWindow")
	procGetMessageW               = user32.NewProc("GetMessageW")
	procTranslateMessage          = user32.NewProc("TranslateMessage")
	procDispatchMessageW          = user32.NewProc("DispatchMessageW")
	procPostMessageW              = user32.NewProc("PostMessageW")
	procSendMessageW              = user32.NewProc("SendMessageW")
	procPostQuitMessage           = user32.NewProc("PostQuitMessage")
	procRegisterWindowMessageW    = user32.NewProc("RegisterWindowMessageW")
	procSetTimer                  = user32.NewProc("SetTimer")
	procKillTimer                 = user32.NewProc("KillTimer")
	procCreatePopupMenu           = user32.NewProc("CreatePopupMenu")
	procAppendMenuW               = user32.NewProc("AppendMenuW")
	procTrackPopupMenu            = user32.NewProc("TrackPopupMenu")
	procDestroyMenu               = user32.NewProc("DestroyMenu")
	procGetCursorPos              = user32.NewProc("GetCursorPos")
	procSetForegroundWindow       = user32.NewProc("SetForegroundWindow")
	procSetWindowPos              = user32.NewProc("SetWindowPos")
	procMessageBoxW               = user32.NewProc("MessageBoxW")
	procGetSystemMetrics          = user32.NewProc("GetSystemMetrics")
	procCreateIconIndirect        = user32.NewProc("CreateIconIndirect")
	procDestroyIcon               = user32.NewProc("DestroyIcon")
	procGetDC                     = user32.NewProc("GetDC")
	procReleaseDC                 = user32.NewProc("ReleaseDC")
	procEnumWindows               = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessId  = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible           = user32.NewProc("IsWindowVisible")
	procGetWindow                 = user32.NewProc("GetWindow")
	procSetProcessDpiAwarenessCtx = user32.NewProc("SetProcessDpiAwarenessContext")

	gdi32                = windows.NewLazySystemDLL("gdi32.dll")
	procCreateDIBSection = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap     = gdi32.NewProc("CreateBitmap")
	procDeleteObject     = gdi32.NewProc("DeleteObject")
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW    = shell32.NewProc("ShellExecuteW")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procCreateActCtxW    = kernel32.NewProc("CreateActCtxW")
	procActivateActCtx   = kernel32.NewProc("ActivateActCtx")
	procCreateMutexW     = kernel32.NewProc("CreateMutexW")
)

const (
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmNull        = 0x0000
	wmTimer       = 0x0113
	wmUser        = 0x0400
	wmApp         = 0x8000
	wmContextMenu = 0x007B
	wmTrayIcon    = wmApp + 1
	wmInvoke      = wmApp + 2

	ninSelect           = wmUser + 0
	ninKeySelect        = wmUser + 1
	ninBalloonUserClick = wmUser + 5

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4
	nifMessage    = 0x01
	nifIcon       = 0x02
	nifTip        = 0x04
	nifInfo       = 0x10
	nifShowTip    = 0x80
	niifNoSound   = 0x10
	notifyIconV4  = 4

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfPopup     = 0x0010
	mfSeparator = 0x0800
	tpmRightBtn = 0x0002
	tpmReturn   = 0x0100

	mbOK          = 0x0000
	mbYesNo       = 0x0004
	mbIconInfo    = 0x0040
	mbIconWarning = 0x0030
	mbIconError   = 0x0010
	mbTopMost     = 0x00040000
	idYes         = 6

	smCxSmIcon = 49
	gwOwner    = 4
	swShow     = 5
	swHide     = 0

	swpNoSize = 0x0001
	swpNoMove = 0x0002
	hwndTop   = ^uintptr(0) // HWND_TOPMOST (-1)
)

type point struct{ X, Y int32 }

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	Private uint32
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

// notifyIconData est NOTIFYICONDATAW (version 4, avec hBalloonIcon).
type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32 // union uTimeout / uVersion
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     uintptr
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

func utf16(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		p, _ = windows.UTF16PtrFromString("?")
	}
	return p
}

// copyUTF16 copie s dans un tableau de taille fixe, tronqué, terminé par zéro.
func copyUTF16(dst []uint16, s string) {
	u := windows.StringToUTF16(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	n := copy(dst, u)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
}

func messageBox(hwnd uintptr, title, text string, flags uintptr) int {
	r, _, _ := procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(utf16(text))), uintptr(unsafe.Pointer(utf16(title))), flags|mbTopMost)
	return int(r)
}

func shellExecute(hwnd uintptr, verb, file, params string, show uintptr) error {
	var v, p uintptr
	if verb != "" {
		v = uintptr(unsafe.Pointer(utf16(verb)))
	}
	if params != "" {
		p = uintptr(unsafe.Pointer(utf16(params)))
	}
	r, _, _ := procShellExecuteW.Call(hwnd, v, uintptr(unsafe.Pointer(utf16(file))), p, 0, show)
	if r <= 32 {
		return fmt.Errorf("ShellExecute %s : erreur %d", file, r)
	}
	return nil
}
