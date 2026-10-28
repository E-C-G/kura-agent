//go:build windows

package tray

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/E-C-G/kura-agent/internal/ipc"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/schedtask"
)

// Fenêtre « Restaurer mes fichiers » : contrôles Win32 standard (comctl32 v6, activé
// par loadTaskDialog sur le thread de l'interface). Elle ne fait qu'afficher et
// transmettre : la tâche \Kura\Restore vérifie tout et écrit au nom du compte.

var (
	procIsDialogMessageW    = user32.NewProc("IsDialogMessageW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procEnableWindow        = user32.NewProc("EnableWindow")
	procSetWindowTextW      = user32.NewProc("SetWindowTextW")
	procGetDpiForSystem     = user32.NewProc("GetDpiForSystem")
	procAdjustWindowRectEx  = user32.NewProc("AdjustWindowRectEx")
	procLoadCursorW         = user32.NewProc("LoadCursorW")
	procCreateFontW         = gdi32.NewProc("CreateFontW")
	procSHBrowseForFolderW  = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDList = shell32.NewProc("SHGetPathFromIDListW")
	ole32                   = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx      = ole32.NewProc("CoInitializeEx")
	procCoTaskMemFree       = ole32.NewProc("CoTaskMemFree")
)

const (
	wmSetFont    = 0x0030
	wmSetIcon    = 0x0080
	wmNotify     = 0x004E
	wmCommand    = 0x0111
	wmSetRedraw  = 0x000B
	bnClicked    = 0
	cbnSelChange = 1

	wsOverlapped     = 0x00000000
	wsCaption        = 0x00C00000
	wsSysMenu        = 0x00080000
	wsMinimizeBox    = 0x00020000
	wsChild          = 0x40000000
	wsVisible        = 0x10000000
	wsTabStop        = 0x00010000
	wsGroup          = 0x00020000
	wsBorder         = 0x00800000
	wsVScroll        = 0x00200000
	wsExClientEdge   = 0x00000200
	wsExControlPar   = 0x00010000
	wsExAppWindow    = 0x00040000
	bsPushButton     = 0x0
	bsAutoRadio      = 0x9
	cbsDropDownList  = 0x3
	ssLeft           = 0x0
	ssEndEllipsis    = 0x4000
	ssPathEllipsis   = 0x8000
	bmGetCheck       = 0xF0
	bmSetCheck       = 0xF1
	cbAddString      = 0x143
	cbResetContent   = 0x14B
	cbGetCurSel      = 0x147
	cbSetCurSel      = 0x14E
	pbmSetPos        = wmUser + 2
	pbmSetRange32    = wmUser + 6
	dtmGetSystemTime = 0x1001
	dtmSetSystemTime = 0x1002
	dtmSetRange      = 0x1004
	dtsLongDate      = 0x0004
	gdtValid         = 0
	gdtrMin          = 1
	gdtrMax          = 2

	lvmGetItemCount      = 0x1004
	lvmDeleteAllItems    = 0x1009
	lvmGetNextItem       = 0x100C
	lvmSetItemState      = 0x102B
	lvmGetItemState      = 0x102C
	lvmSetItemCount      = 0x102F
	lvmSetExtendedStyle  = 0x1036
	lvmInsertItemW       = 0x104D
	lvmInsertColumnW     = 0x1061
	lvmSetItemTextW      = 0x1074
	lvsReport            = 0x0001
	lvsShowSelAlways     = 0x0008
	lvsSingleSel         = 0x0004
	lvsExCheckBoxes      = 0x0004
	lvsExFullRowSelect   = 0x0020
	lvsExDoubleBuffer    = 0x00010000
	lvifText             = 0x0001
	lvisSelected         = 0x0002
	lvisStateImageMask   = 0xF000
	lvniSelected         = 0x0002
	lvcfFmt              = 0x0001
	lvcfWidth            = 0x0002
	lvcfText             = 0x0004
	lvcfmtRight          = 0x0001
	lvnItemChanged       = 0xFFFFFF9B // LVN_ITEMCHANGED (-101)
	iccListView          = 0x0001
	iccProgress          = 0x0020
	iccDate              = 0x0100
	iccStandard          = 0x4000
	bifReturnOnlyFSDirs  = 0x0001
	bifNewDialogStyle    = 0x0040
	bifNoNewFolderButton = 0x0200
	bffmInitialized      = 1
	bffmSetSelectionW    = wmUser + 103

	rTimerHistory = 1
	rTimerCount   = 2
)

// Identifiants des contrôles.
const (
	idFolder = 201 + iota
	idBrowse
	idDate
	idShow
	idList
	idHistory
	idCheckAll
	idUncheckAll
	idModeCopy
	idModeMissing
	idRestore
	idClose
	idProgress
	idStatus
	idOpenDir
	idCount
	idLabel
)

// maxRows : au-delà, la liste ne montre que les premiers (le serveur en envoie 50 000 au plus).
const maxRows = 50000

type rect struct{ Left, Top, Right, Bottom int32 }

type lvColumn struct {
	Mask      uint32
	Fmt       int32
	Cx        int32
	Text      *uint16
	TextMax   int32
	SubItem   int32
	Image     int32
	Order     int32
	CxMin     int32
	CxDefault int32
	CxIdeal   int32
}

type lvItem struct {
	Mask      uint32
	Item      int32
	SubItem   int32
	State     uint32
	StateMask uint32
	Text      *uint16
	TextMax   int32
	Image     int32
	LParam    uintptr
	Indent    int32
	GroupID   int32
	Columns   uint32
	PuColumns uintptr
	PiColFmt  uintptr
	Group     int32
}

type nmhdr struct {
	HwndFrom uintptr
	IDFrom   uintptr
	Code     uint32
}

type nmListView struct {
	Hdr      nmhdr
	Item     int32
	SubItem  int32
	NewState uint32
	OldState uint32
	Changed  uint32
	Action   point
	LParam   uintptr
}

type browseInfo struct {
	Owner       uintptr
	Root        uintptr
	DisplayName *uint16
	Title       *uint16
	Flags       uint32
	Fn          uintptr
	LParam      uintptr
	Image       int32
}

type initCommonControlsEx struct {
	Size uint32
	ICC  uint32
}

// restoreWin : la fenêtre ouverte (une à la fois, thread de l'interface).
type restoreWin struct {
	a    *app
	hwnd uintptr
	font uintptr
	dpi  int32
	ctl  map[int]uintptr

	roots    []string // raccourcis envoyés par la tâche, puis dossiers parcourus
	folder   string   // dossier affiché
	files    []ipc.RestoreFile
	at       *time.Time
	account  string
	running  bool
	waiting  bool // une requête attend sa réponse
	lastDir  string
	selected int

	// Canal (goroutines).
	mu       sync.Mutex
	listener *ipc.Listener
	peer     *ipc.Peer
	chunks   map[int][]ipc.RestoreFile
	listSeq  int // numéro de la liste attendue : les lots d'une liste abandonnée sont écartés
	ctx      context.Context
	cancel   context.CancelFunc
}

var (
	rw          *restoreWin // thread de l'interface
	rwClassOnce sync.Once
	rwClassErr  error
)

var restoreWndProc = windows.NewCallback(func(hwnd, m, wParam, lParam uintptr) uintptr {
	if w := rw; w != nil && (w.hwnd == hwnd || w.hwnd == 0) {
		if r, ok := w.handle(hwnd, uint32(m), wParam, lParam); ok {
			return r
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wParam, lParam)
	return r
})

// restoreDialogHwnd : fenêtre à passer à IsDialogMessage (tabulation entre contrôles).
func restoreDialogHwnd() uintptr {
	if rw != nil {
		return rw.hwnd
	}
	return 0
}

// openRestore ouvre la fenêtre (ou la ramène au premier plan) et lance la tâche.
func (a *app) openRestore() {
	if rw != nil && rw.hwnd != 0 {
		procShowWindow.Call(rw.hwnd, 9) // SW_RESTORE
		procSetForegroundWindow.Call(rw.hwnd)
		return
	}
	w, err := newRestoreWin(a)
	if err != nil {
		a.log.Warn("fenêtre de restauration", "err", err)
		messageBox(a.hwnd, "Kura", "Fenêtre de restauration indisponible : "+err.Error(), mbOK|mbIconError)
		return
	}
	w.status("Connexion au service de restauration…")
	go w.connect()
}

func registerRestoreClass() error {
	rwClassOnce.Do(func() {
		inst, _, _ := procGetModuleHandleW.Call(0)
		cursor, _, _ := procLoadCursorW.Call(0, 32512) // IDC_ARROW
		wc := wndClassEx{Size: uint32(unsafe.Sizeof(wndClassEx{})), WndProc: restoreWndProc, Instance: inst,
			Cursor: cursor, Background: 16, ClassName: utf16("KuraRestoreWindow")} // COLOR_BTNFACE + 1
		if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			rwClassErr = fmt.Errorf("RegisterClassEx : %v", err)
		}
	})
	return rwClassErr
}

func initCommonControls() {
	lib, err := windows.LoadLibrary("comctl32.dll") // v6 : contexte d'activation de loadTaskDialog
	if err != nil {
		return
	}
	proc, err := windows.GetProcAddress(lib, "InitCommonControlsEx")
	if err != nil {
		return
	}
	icc := initCommonControlsEx{Size: uint32(unsafe.Sizeof(initCommonControlsEx{})), ICC: iccListView | iccProgress | iccDate | iccStandard}
	syscallN(proc, uintptr(unsafe.Pointer(&icc)))
}

// newRestoreWin crée la fenêtre et ses contrôles, sans la connecter.
func newRestoreWin(a *app) (*restoreWin, error) {
	if err := registerRestoreClass(); err != nil {
		return nil, err
	}
	initCommonControls()
	procCoInitializeEx.Call(0, 2) // COINIT_APARTMENTTHREADED : sélecteur de dossier
	w := &restoreWin{a: a, ctl: map[int]uintptr{}, chunks: map[int][]ipc.RestoreFile{}, selected: -1}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	dpi := uintptr(96)
	if procGetDpiForSystem.Find() == nil { // Windows 10 1607 et suivants
		if d, _, _ := procGetDpiForSystem.Call(); d != 0 {
			dpi = d
		}
	}
	w.dpi = int32(dpi)
	rw = w

	const style = wsOverlapped | wsCaption | wsSysMenu | wsMinimizeBox
	const exStyle = wsExControlPar | wsExAppWindow
	r := rect{0, 0, w.s(880), w.s(620)}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), style, 0, exStyle)
	width, height := r.Right-r.Left, r.Bottom-r.Top
	sx, _, _ := procGetSystemMetrics.Call(0)
	sy, _, _ := procGetSystemMetrics.Call(1)
	x, y := (int32(sx)-width)/2, (int32(sy)-height)/2
	inst, _, _ := procGetModuleHandleW.Call(0)
	h, _, err := procCreateWindowExW.Call(exStyle, uintptr(unsafe.Pointer(utf16("KuraRestoreWindow"))),
		uintptr(unsafe.Pointer(utf16(RestoreTitle))), style, uintptr(x), uintptr(y), uintptr(width), uintptr(height), 0, 0, inst, 0)
	if h == 0 {
		rw = nil
		return nil, fmt.Errorf("CreateWindowEx : %v", err)
	}
	w.hwnd = h
	f, _, _ := procCreateFontW.Call(uintptr(-w.s(12)), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(utf16("Segoe UI"))))
	w.font = f
	if a != nil {
		if ic := a.iconHandle(); ic != 0 {
			procSendMessageW.Call(h, wmSetIcon, 0, ic)
			procSendMessageW.Call(h, wmSetIcon, 1, ic)
		}
	}
	w.build()
	w.enable(false)
	procShowWindow.Call(h, swShow)
	procSetForegroundWindow.Call(h)
	return w, nil
}

// s met à l'échelle une mesure donnée à 96 ppp.
func (w *restoreWin) s(v int32) int32 { return v * w.dpi / 96 }

func (w *restoreWin) add(id int, class, text string, style, exStyle uint32, x, y, cx, cy int32) uintptr {
	inst, _, _ := procGetModuleHandleW.Call(0)
	h, _, _ := procCreateWindowExW.Call(uintptr(exStyle), uintptr(unsafe.Pointer(utf16(class))), uintptr(unsafe.Pointer(utf16(text))),
		uintptr(wsChild|wsVisible|style), uintptr(w.s(x)), uintptr(w.s(y)), uintptr(w.s(cx)), uintptr(w.s(cy)), w.hwnd, uintptr(id), inst, 0)
	if h != 0 && w.font != 0 {
		procSendMessageW.Call(h, wmSetFont, w.font, 1)
	}
	if id != 0 {
		w.ctl[id] = h
	}
	return h
}

func (w *restoreWin) build() {
	w.add(0, "STATIC", "Dossier :", ssLeft, 0, 12, 16, 80, 20)
	w.add(idFolder, "COMBOBOX", "", cbsDropDownList|wsVScroll|wsTabStop, 0, 96, 12, 640, 300)
	w.add(idBrowse, "BUTTON", "Parcourir…", bsPushButton|wsTabStop, 0, 744, 11, 124, 26)

	w.add(0, "STATIC", "État au :", ssLeft, 0, 12, 50, 80, 20)
	w.add(idDate, "SysDateTimePick32", "", dtsLongDate|wsTabStop, 0, 96, 46, 240, 24)
	w.add(idLabel, "STATIC", "fin de journée ; aujourd'hui = dernière sauvegarde ; 90 jours au plus", ssLeft, 0, 346, 50, 390, 20)
	w.add(idShow, "BUTTON", "Afficher", bsPushButton|wsTabStop, 0, 744, 45, 124, 26)

	list := w.add(idList, "SysListView32", "", lvsReport|lvsShowSelAlways|lvsSingleSel|wsTabStop|wsBorder, wsExClientEdge, 12, 82, 856, 300)
	procSendMessageW.Call(list, lvmSetExtendedStyle, 0, lvsExCheckBoxes|lvsExFullRowSelect|lvsExDoubleBuffer)
	w.column(list, 0, "Fichier", 440, false)
	w.column(list, 1, "Taille", 90, true)
	w.column(list, 2, "Sauvegardé le", 130, false)
	w.column(list, 3, "État", 170, false)

	w.add(0, "STATIC", "Versions du fichier sélectionné :", ssLeft, 0, 12, 390, 400, 20)
	hist := w.add(idHistory, "SysListView32", "", lvsReport|lvsSingleSel|wsBorder, wsExClientEdge, 12, 410, 856, 92)
	procSendMessageW.Call(hist, lvmSetExtendedStyle, 0, lvsExFullRowSelect|lvsExDoubleBuffer)
	w.column(hist, 0, "Date de la sauvegarde", 200, false)
	w.column(hist, 1, "Taille", 90, true)
	w.column(hist, 2, "État", 300, false)

	w.add(idCheckAll, "BUTTON", "Tout cocher", bsPushButton|wsTabStop, 0, 12, 512, 110, 26)
	w.add(idUncheckAll, "BUTTON", "Tout décocher", bsPushButton|wsTabStop, 0, 128, 512, 110, 26)
	w.add(idCount, "STATIC", "", ssLeft, 0, 248, 517, 240, 20)
	w.add(idModeCopy, "BUTTON", "Dans un nouveau dossier « Restauration Kura … » à côté", bsAutoRadio|wsGroup|wsTabStop, 0, 496, 508, 372, 22)
	w.add(idModeMissing, "BUTTON", "À leur place (seulement les fichiers supprimés)", bsAutoRadio, 0, 496, 530, 372, 22)
	procSendMessageW.Call(w.ctl[idModeCopy], bmSetCheck, 1, 0)

	w.add(idProgress, "msctls_progress32", "", 0, 0, 12, 562, 520, 16)
	w.add(idStatus, "STATIC", "", ssLeft|ssEndEllipsis, 0, 12, 586, 520, 20)
	w.add(idOpenDir, "BUTTON", "Ouvrir le dossier", bsPushButton|wsTabStop, 0, 540, 578, 124, 28)
	procShowWindow.Call(w.ctl[idOpenDir], swHide)
	w.add(idRestore, "BUTTON", "Restaurer", bsPushButton|wsTabStop, 0, 672, 578, 96, 28)
	w.add(idClose, "BUTTON", "Fermer", bsPushButton|wsTabStop, 0, 772, 578, 96, 28)

	// Calendrier borné aux 90 derniers jours.
	now := time.Now()
	rng := [2]windows.Systemtime{sysTime(now.AddDate(0, 0, -90)), sysTime(now)}
	procSendMessageW.Call(w.ctl[idDate], dtmSetRange, gdtrMin|gdtrMax, uintptr(unsafe.Pointer(&rng[0])))
	st := sysTime(now)
	procSendMessageW.Call(w.ctl[idDate], dtmSetSystemTime, gdtValid, uintptr(unsafe.Pointer(&st)))
}

func sysTime(t time.Time) windows.Systemtime {
	return windows.Systemtime{Year: uint16(t.Year()), Month: uint16(t.Month()), DayOfWeek: uint16(t.Weekday()), Day: uint16(t.Day())}
}

func (w *restoreWin) column(list uintptr, i int, title string, width int32, right bool) {
	c := lvColumn{Mask: lvcfText | lvcfWidth | lvcfFmt, Cx: w.s(width), Text: utf16(title)}
	if right {
		c.Fmt = lvcfmtRight
	}
	procSendMessageW.Call(list, lvmInsertColumnW, uintptr(i), uintptr(unsafe.Pointer(&c)))
}

func (w *restoreWin) setText(id int, s string) {
	procSetWindowTextW.Call(w.ctl[id], uintptr(unsafe.Pointer(utf16(s))))
}

func (w *restoreWin) status(s string) { w.setText(idStatus, s) }

// enable active ou grise les commandes (pendant une requête ou une restauration).
func (w *restoreWin) enable(on bool) {
	v := uintptr(0)
	if on {
		v = 1
	}
	for _, id := range []int{idFolder, idBrowse, idDate, idShow, idCheckAll, idUncheckAll, idModeCopy, idModeMissing, idRestore} {
		procEnableWindow.Call(w.ctl[id], v)
	}
}

// --- liste ----------------------------------------------------------------------

func (w *restoreWin) setRow(list uintptr, i int, cols []string) {
	it := lvItem{Mask: lvifText, Item: int32(i), Text: utf16(cols[0])}
	procSendMessageW.Call(list, lvmInsertItemW, 0, uintptr(unsafe.Pointer(&it)))
	for c := 1; c < len(cols); c++ {
		sub := lvItem{SubItem: int32(c), Text: utf16(cols[c])}
		procSendMessageW.Call(list, lvmSetItemTextW, uintptr(i), uintptr(unsafe.Pointer(&sub)))
	}
}

// populate remplit la liste (sans tri : la ligne i est le fichier i).
func (w *restoreWin) populate(files []ipc.RestoreFile) {
	if len(files) > maxRows {
		files = files[:maxRows]
	}
	w.files = files
	w.selected = -1
	list := w.ctl[idList]
	procSendMessageW.Call(list, wmSetRedraw, 0, 0)
	procSendMessageW.Call(list, lvmDeleteAllItems, 0, 0)
	procSendMessageW.Call(list, lvmSetItemCount, uintptr(len(files)), 0)
	for i, f := range files {
		r := FileRow(f)
		w.setRow(list, i, r[:])
	}
	procSendMessageW.Call(list, wmSetRedraw, 1, 0)
	procSendMessageW.Call(w.ctl[idHistory], lvmDeleteAllItems, 0, 0)
	w.updateCount()
}

func (w *restoreWin) setChecked(i int, on bool) {
	v := uint32(1)
	if on {
		v = 2
	}
	it := lvItem{StateMask: lvisStateImageMask, State: v << 12}
	procSendMessageW.Call(w.ctl[idList], lvmSetItemState, uintptr(i), uintptr(unsafe.Pointer(&it)))
}

// setAll coche ou décoche tout (sauf les fichiers non restaurables).
func (w *restoreWin) setAll(on bool) {
	for i, f := range w.files {
		w.setChecked(i, on && f.Problem == "")
	}
	w.updateCount()
}

// checked renvoie les lignes cochées restaurables.
func (w *restoreWin) checked() []int {
	var out []int
	for i, f := range w.files {
		st, _, _ := procSendMessageW.Call(w.ctl[idList], lvmGetItemState, uintptr(i), lvisStateImageMask)
		if (st&lvisStateImageMask)>>12 == 2 && f.Problem == "" {
			out = append(out, i)
		}
	}
	return out
}

func (w *restoreWin) updateCount() {
	c := w.checked()
	if len(w.files) == 0 {
		w.setText(idCount, "")
		return
	}
	w.setText(idCount, fmt.Sprintf("%d coché(s), %s", len(c), HumanSize(PickSize(w.files, c))))
}

func (w *restoreWin) selectedRow() int {
	r, _, _ := procSendMessageW.Call(w.ctl[idList], lvmGetNextItem, ^uintptr(0), lvniSelected)
	return int(int32(r))
}

func (w *restoreWin) showVersions(vs []ipc.RestoreVersion) {
	h := w.ctl[idHistory]
	procSendMessageW.Call(h, lvmDeleteAllItems, 0, 0)
	for i, v := range vs {
		r := VersionRow(v)
		w.setRow(h, i, r[:])
	}
}

// chosenDate : état demandé d'après le calendrier.
func (w *restoreWin) chosenDate() *time.Time {
	var st windows.Systemtime
	if r, _, _ := procSendMessageW.Call(w.ctl[idDate], dtmGetSystemTime, 0, uintptr(unsafe.Pointer(&st))); r != gdtValid {
		return nil
	}
	now := time.Now()
	return StateAt(time.Date(int(st.Year), time.Month(st.Month), int(st.Day), 0, 0, 0, 0, now.Location()), now)
}

func (w *restoreWin) mode() string {
	if r, _, _ := procSendMessageW.Call(w.ctl[idModeMissing], bmGetCheck, 0, 0); r == 1 {
		return ipc.RestoreMissing
	}
	return ipc.RestoreCopy
}

func (w *restoreWin) setRoots(roots []string, sel string) {
	c := w.ctl[idFolder]
	procSendMessageW.Call(c, cbResetContent, 0, 0)
	idx := -1
	for i, r := range roots {
		procSendMessageW.Call(c, cbAddString, 0, uintptr(unsafe.Pointer(utf16(r))))
		if strings.EqualFold(r, sel) {
			idx = i
		}
	}
	w.roots = roots
	procSendMessageW.Call(c, cbSetCurSel, uintptr(idx), 0)
}

// --- messages Windows -----------------------------------------------------------

func (w *restoreWin) handle(hwnd uintptr, m uint32, wParam, lParam uintptr) (uintptr, bool) {
	switch m {
	case wmCommand:
		id, code := int(wParam&0xFFFF), int(wParam>>16&0xFFFF)
		switch {
		case id == idBrowse && code == bnClicked:
			if dir := w.browse(); dir != "" {
				w.choose(dir)
			}
		case id == idFolder && code == cbnSelChange:
			i, _, _ := procSendMessageW.Call(w.ctl[idFolder], cbGetCurSel, 0, 0)
			if int(int32(i)) >= 0 && int(i) < len(w.roots) {
				w.choose(w.roots[i])
			}
		case id == idShow && code == bnClicked:
			w.list()
		case id == idCheckAll && code == bnClicked:
			w.setAll(true)
		case id == idUncheckAll && code == bnClicked:
			w.setAll(false)
		case id == idRestore && code == bnClicked:
			w.run()
		case id == idOpenDir && code == bnClicked:
			if w.lastDir != "" {
				_ = shellExecute(w.hwnd, "open", w.lastDir, "", swShow)
			}
		case id == idClose && code == bnClicked:
			procSendMessageW.Call(w.hwnd, wmClose, 0, 0)
		}
		return 0, true
	case wmNotify:
		hdr := *(**nmhdr)(unsafe.Pointer(&lParam))
		if int(hdr.IDFrom) == idList && hdr.Code == lvnItemChanged {
			nm := *(**nmListView)(unsafe.Pointer(&lParam))
			if nm.Changed&0x8 != 0 { // LVIF_STATE
				if nm.NewState&lvisSelected != 0 && nm.OldState&lvisSelected == 0 {
					procSetTimer.Call(w.hwnd, rTimerHistory, 300, 0)
				}
				if (nm.NewState^nm.OldState)&lvisStateImageMask != 0 {
					procSetTimer.Call(w.hwnd, rTimerCount, 150, 0)
				}
			}
		}
		return 0, true
	case wmTimer:
		procKillTimer.Call(w.hwnd, wParam)
		switch wParam {
		case rTimerHistory:
			w.history()
		case rTimerCount:
			w.updateCount()
		}
		return 0, true
	case wmClose:
		if w.running && messageBox(w.hwnd, RestoreTitle, "Une restauration est en cours. Fermer quand même ?\n\nLes fichiers déjà restaurés restent ; les autres ne le seront pas.",
			mbYesNo|mbIconWarning) != idYes {
			return 0, true
		}
		procDestroyWindow.Call(w.hwnd)
		return 0, true
	case wmDestroy:
		w.cancel()
		w.mu.Lock()
		p, l := w.peer, w.listener
		w.mu.Unlock()
		// Hors du thread de l'interface : fermer le canal peut attendre la tâche.
		go func() {
			if p != nil {
				p.Close()
			}
			if l != nil {
				_ = l.Close()
			}
		}()
		if w.font != 0 {
			procDeleteObject.Call(w.font)
		}
		if rw == w {
			rw = nil
		}
		return 0, true
	}
	return 0, false
}

// browse ouvre le sélecteur de dossier de Windows.
func (w *restoreWin) browse() string {
	name := make([]uint16, windows.MAX_PATH)
	start := w.folder
	if start == "" && len(w.roots) > 0 {
		start = w.roots[0]
	}
	browseStart = utf16(start)
	bi := browseInfo{Owner: w.hwnd, DisplayName: &name[0], Title: utf16("Dossier à restaurer (ou dossier qui contenait les fichiers supprimés)"),
		Flags: bifReturnOnlyFSDirs | bifNewDialogStyle | bifNoNewFolderButton, Fn: browseCallback}
	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer procCoTaskMemFree.Call(pidl)
	buf := make([]uint16, 32768)
	if r, _, _ := procSHGetPathFromIDList.Call(pidl, uintptr(unsafe.Pointer(&buf[0]))); r == 0 {
		messageBox(w.hwnd, RestoreTitle, "Ce choix n'est pas un dossier du disque.", mbOK|mbIconWarning)
		return ""
	}
	return windows.UTF16ToString(buf)
}

var browseStart *uint16 // dossier présélectionné (thread de l'interface)

var browseCallback = windows.NewCallback(func(hwnd, m, lParam, data uintptr) uintptr {
	if m == bffmInitialized && browseStart != nil {
		procSendMessageW.Call(hwnd, bffmSetSelectionW, 1, uintptr(unsafe.Pointer(browseStart)))
	}
	return 0
})

// choose affiche un dossier : ajouté aux raccourcis s'il n'y est pas, puis listé.
func (w *restoreWin) choose(dir string) {
	found := false
	for _, r := range w.roots {
		found = found || strings.EqualFold(r, dir)
	}
	roots := w.roots
	if !found {
		roots = append(append([]string(nil), w.roots...), dir)
	}
	w.setRoots(roots, dir)
	w.folder = dir
	w.list()
}

// --- dialogue avec la tâche -----------------------------------------------------

// connect publie le canal, lance la tâche \Kura\Restore et attend qu'elle se joigne.
func (w *restoreWin) connect() {
	a := w.a
	fail := func(text string) {
		a.invoke(func() {
			if rw == w {
				w.status(text)
				messageBox(w.hwnd, RestoreTitle, text, mbOK|mbIconError)
			}
		})
	}
	l, err := ipc.ListenRestore()
	if err != nil {
		a.log.Warn("canal de restauration", "err", err)
		fail("Canal de restauration indisponible : " + err.Error())
		return
	}
	w.mu.Lock()
	w.listener = l
	w.mu.Unlock()
	if w.ctx.Err() != nil {
		_ = l.Close()
		return
	}
	launch := func() ([]byte, error) {
		cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "schtasks.exe"), "/Run", "/TN", schedtask.RestoreName)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		return cmd.CombinedOutput()
	}
	if out, err := launch(); err != nil {
		a.log.Warn("lancement de la tâche de restauration", "err", err, "sortie", strings.TrimSpace(string(out)))
		fail("Lancement impossible de la tâche " + schedtask.RestoreName + " :\n" + strings.TrimSpace(string(out)) +
			"\n\nElle est créée par « kura-agent install » (version 0.15.0 ou plus) pour les comptes de tray.users.")
		_ = l.Close()
		return
	}
	// La tâche se joint en quelques secondes ; au-delà d'une minute, on abandonne.
	// Relance à 15 s : une tâche qui finissait au moment du premier lancement l'a ignoré.
	timer := time.AfterFunc(time.Minute, func() { _ = l.Close() })
	retry := time.AfterFunc(15*time.Second, func() {
		if w.currentPeer() == nil && w.ctx.Err() == nil {
			_, _ = launch()
		}
	})
	defer retry.Stop()
	for {
		conn, err := l.Accept()
		if err != nil {
			timer.Stop()
			if w.ctx.Err() == nil {
				fail("La tâche de restauration ne répond pas. Réessayer, ou voir le journal restore-tray.log dans " + paths.LogsDir() + ".")
			}
			return
		}
		// Seule la tâche (SYSTEM) est servie : un autre programme du compte est refusé.
		if c, ok := conn.(interface{ ClientPrivileged() bool }); !ok || !c.ClientPrivileged() {
			a.log.Warn("connexion au canal de restauration refusée : ni SYSTEM ni administrateur")
			conn.Close()
			continue
		}
		timer.Stop()
		p := ipc.NewPeer(conn, w.onMsg)
		w.mu.Lock()
		w.peer = p
		w.mu.Unlock()
		<-p.Done()
		a.invoke(func() {
			if rw == w {
				w.running, w.waiting = false, false
				w.enable(false)
				w.status("Le service de restauration s'est arrêté : fermer la fenêtre et la rouvrir.")
			}
		})
		return
	}
}

// onMsg : messages sans réponse attendue (goroutine de lecture).
func (w *restoreWin) onMsg(m ipc.Msg) {
	switch m.Type {
	case ipc.TypeRestoreHello:
		w.a.invoke(func() {
			if rw != w {
				return
			}
			if m.Message != "" {
				w.status(m.Message)
				messageBox(w.hwnd, RestoreTitle, m.Message, mbOK|mbIconWarning)
				return
			}
			if m.Restore != nil {
				w.account = m.Restore.Account
				w.setRoots(m.Restore.Roots, "")
			}
			w.enable(true)
			w.status("Choisir un dossier : ses fichiers sauvegardés s'affichent, à cocher.")
		})
	case ipc.TypeRestoreFiles:
		if m.Restore != nil {
			w.mu.Lock()
			if m.Restore.Req == w.listSeq {
				w.chunks[m.Restore.Req] = append(w.chunks[m.Restore.Req], m.Restore.Files...)
			}
			w.mu.Unlock()
		}
	case ipc.TypeRestoreProgress:
		if r := m.Restore; r != nil {
			w.a.invoke(func() {
				if rw != w || !w.running {
					return
				}
				procSendMessageW.Call(w.ctl[idProgress], pbmSetRange32, 0, uintptr(r.Total))
				procSendMessageW.Call(w.ctl[idProgress], pbmSetPos, uintptr(r.Done), 0)
				w.status(fmt.Sprintf("Restauration : %d/%d fichier(s), %s…", r.Done, r.Total, HumanSize(r.Bytes)))
			})
		}
	}
}

func (w *restoreWin) currentPeer() *ipc.Peer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peer
}

// request envoie une requête hors du thread de l'interface et rend la réponse sur lui.
func (w *restoreWin) request(m ipc.Msg, timeout time.Duration, then func(ipc.Msg, error)) {
	p := w.currentPeer()
	if p == nil {
		then(ipc.Msg{}, errors.New("service de restauration non joint"))
		return
	}
	go func() {
		ctx, cancel := w.ctx, func() {}
		if timeout > 0 {
			ctx, cancel = context.WithTimeout(w.ctx, timeout)
		}
		defer cancel()
		r, err := p.Request(ctx, m)
		w.a.invoke(func() {
			if rw == w {
				then(r, err)
			}
		})
	}()
}

func (w *restoreWin) list() {
	if w.waiting || w.running {
		return
	}
	if w.folder == "" {
		w.status("Choisir d'abord un dossier.")
		return
	}
	w.at = w.chosenDate()
	w.waiting = true
	w.enable(false)
	w.populate(nil)
	procShowWindow.Call(w.ctl[idOpenDir], swHide)
	w.status("Lecture des sauvegardes de " + w.folder + "…")
	w.mu.Lock()
	w.listSeq++
	seq := w.listSeq
	w.chunks = map[int][]ipc.RestoreFile{}
	w.mu.Unlock()
	m := ipc.Msg{Type: ipc.TypeRestoreList, Path: w.folder, Restore: &ipc.RestoreData{At: w.at, Req: seq}}
	p := w.currentPeer()
	if p == nil {
		w.waiting = false
		w.status("Service de restauration non joint.")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(w.ctx, 15*time.Minute)
		defer cancel()
		r, err := p.Request(ctx, m)
		w.mu.Lock()
		files := w.chunks[seq]
		delete(w.chunks, seq)
		w.mu.Unlock()
		w.a.invoke(func() {
			if rw != w {
				return
			}
			w.waiting = false
			w.enable(true)
			switch {
			case err != nil:
				w.status("Liste impossible : " + err.Error())
			case r.Message != "" && (r.Restore == nil || r.Restore.Total == 0):
				w.status(r.Message)
				messageBox(w.hwnd, RestoreTitle, r.Message, mbOK|mbIconWarning)
			default:
				w.populate(files)
				text := ListSummary(r.Restore.Total, r.Restore.Bytes, w.at)
				if r.Message != "" {
					text = r.Message
				}
				w.status(text)
			}
		})
	}()
}

func (w *restoreWin) history() {
	i := w.selectedRow()
	if i < 0 || i >= len(w.files) || i == w.selected || w.running {
		return
	}
	w.selected = i
	rel := w.files[i].Rel
	w.request(ipc.Msg{Type: ipc.TypeRestoreHistory, Restore: &ipc.RestoreData{Rel: rel}}, time.Minute, func(r ipc.Msg, err error) {
		if err != nil || r.Restore == nil || w.selected != i {
			return
		}
		w.showVersions(r.Restore.Versions)
	})
}

func (w *restoreWin) run() {
	if w.waiting || w.running {
		return
	}
	picked := w.checked()
	if len(picked) == 0 {
		messageBox(w.hwnd, RestoreTitle, "Cocher au moins un fichier.", mbOK|mbIconInfo)
		return
	}
	mode := w.mode()
	if messageBox(w.hwnd, RestoreTitle, ConfirmText(len(picked), PickSize(w.files, picked), w.folder, mode), mbYesNo|mbIconInfo) != idYes {
		return
	}
	p := w.currentPeer()
	if p == nil {
		w.status("Service de restauration non joint.")
		return
	}
	rels := make([]string, len(picked))
	for i, k := range picked {
		rels[i] = w.files[k].Rel
	}
	for _, chunk := range ipc.ChunkStrings(rels) {
		if err := p.Send(ipc.Msg{Type: ipc.TypeRestorePick, Restore: &ipc.RestoreData{Rels: chunk}}); err != nil {
			w.status("Envoi impossible : " + err.Error())
			return
		}
	}
	w.running = true
	w.enable(false)
	procShowWindow.Call(w.ctl[idOpenDir], swHide)
	procSendMessageW.Call(w.ctl[idProgress], pbmSetRange32, 0, uintptr(len(picked)))
	procSendMessageW.Call(w.ctl[idProgress], pbmSetPos, 0, 0)
	w.status("Restauration en cours…")
	w.request(ipc.Msg{Type: ipc.TypeRestoreRun, Restore: &ipc.RestoreData{Mode: mode}}, 0, func(r ipc.Msg, err error) {
		w.running = false
		w.enable(true)
		if err != nil {
			w.status("Restauration interrompue : " + err.Error())
			return
		}
		res := ipc.RestoreData{}
		if r.Restore != nil {
			res = *r.Restore
		}
		title, text := DoneText(r.OK, res, r.Message)
		w.status(text)
		if res.Dir != "" && (res.Done > 0 || res.Kept > 0) {
			w.lastDir = res.Dir
			procShowWindow.Call(w.ctl[idOpenDir], swShow)
		}
		level := LevelInfo
		if !r.OK {
			level = LevelWarning
		}
		dir := w.lastDir
		if res.Failed > 0 && res.Log != "" {
			dir = filepath.Dir(res.Log)
		}
		w.a.balloon(Toast{Title: title, Text: truncate(text, 250), Level: level}, dir)
		w.a.log.Info("restauration depuis l'icône", "dossier", w.folder, "mode", mode, "restaurés", res.Done, "échecs", res.Failed)
	})
}
