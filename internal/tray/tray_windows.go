//go:build windows

package tray

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/E-C-G/kura-agent/internal/ipc"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/schedtask"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

const (
	timerReload  = 1
	timerAnimate = 2
	timerSoon    = 3
	reloadEvery  = 30 * time.Second

	// Commandes du menu.
	cmdRunNow = 100 + iota
	cmdCancel
	cmdSuspend1h
	cmdSuspendTomorrow
	cmdResume
	cmdOpenLogs
	cmdOpenConfig
	cmdCheckConfig
	cmdAbout
	cmdQuit
)

type app struct {
	opt     Options
	log     *slog.Logger
	hwnd    uintptr
	user    string
	exe     string
	session uint32

	// Thread de l'interface uniquement.
	cfg         *config.Config
	cfgErr      error
	cfgMod      time.Time
	status      *state.Status
	seenStatus  bool
	view        View
	icons       map[[2]int]uintptr
	frame       int
	iconAdded   bool
	balloonDir  string // dossier ouvert par un clic sur la dernière notification
	lastLate    time.Time
	taskbarMsg  uintptr
	currentIcon uintptr

	mu    sync.Mutex
	queue []func()
	peer  *ipc.Peer // exécution connectée, s'il y en a une

	promptMu  sync.Mutex // un préavis à la fois
	promptSeq int        // numéro du préavis affiché (sous mu)
	listener  *ipc.Listener
}

// Main lance l'icône dans la session courante. Elle se ferme aussitôt pour un compte
// absent de tray.users, ou si une icône tourne déjà dans la session.
func Main(opt Options) int {
	runtime.LockOSThread() // fenêtres, menus et dialogues vivent sur ce thread

	a := &app{opt: opt, icons: map[[2]int]uintptr{}}
	a.log = trayLogger()
	u, err := user.Current()
	if err != nil {
		a.log.Error("utilisateur inconnu", "err", err)
		return 1
	}
	a.user = u.Username
	tr, err := config.ReadTray(opt.ConfigFile)
	if err != nil {
		a.log.Info("configuration absente ou illisible : pas d'icône", "err", err)
		return 0
	}
	if !config.Listed(tr.Users, a.user) {
		return 0 // compte non concerné : rien à afficher
	}
	name := utf16(`Local\Kura-Tray`)
	m, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if m == 0 || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return 0 // déjà une icône dans cette session
	}
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &a.session)
	a.exe, _ = os.Executable()
	procSetProcessDpiAwarenessCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)

	if _, err := loadTaskDialog(); err != nil {
		a.log.Warn("fenêtre de préavis indisponible (comctl32 v6)", "err", err)
	}
	if err := a.createWindow(); err != nil {
		a.log.Error("fenêtre de l'icône", "err", err)
		return 1
	}
	a.log.Info("icône démarrée", "utilisateur", a.user, "session", a.session, "version", opt.Version)
	a.reload()
	a.addIcon()
	procSetTimer.Call(a.hwnd, timerReload, uintptr(reloadEvery/time.Millisecond), 0)
	go a.serve()

	var m2 msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m2)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m2)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m2)))
	}
	a.removeIcon()
	a.mu.Lock()
	if a.listener != nil {
		_ = a.listener.Close()
	}
	a.mu.Unlock()
	return 0
}

// trayLogger écrit dans %LOCALAPPDATA%\Kura\tray.log (remis à zéro au-delà de 1 Mo).
func trayLogger() *slog.Logger {
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "Kura")
	_ = os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "tray.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 1<<20 {
		_ = os.Remove(p)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(f, nil))
}

// --- fenêtre et boucle de messages -------------------------------------------

var theApp *app // une seule icône par processus

var wndProc = windows.NewCallback(func(hwnd, m, wParam, lParam uintptr) uintptr {
	if a := theApp; a != nil {
		if r, handled := a.handle(hwnd, uint32(m), wParam, lParam); handled {
			return r
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wParam, lParam)
	return r
})

func (a *app) createWindow() error {
	theApp = a
	inst, _, _ := procGetModuleHandleW.Call(0)
	class := utf16("KuraTrayWindow")
	wc := wndClassEx{Size: uint32(unsafe.Sizeof(wndClassEx{})), WndProc: wndProc, Instance: inst, ClassName: class}
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("RegisterClassEx : %v", err)
	}
	// Fenêtre de premier niveau jamais affichée (une fenêtre « message-only » ne
	// recevrait pas TaskbarCreated, envoyé quand l'Explorateur redémarre).
	h, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(utf16("Kura"))),
		0, 0, 0, 0, 0, 0, 0, inst, 0)
	if h == 0 {
		return fmt.Errorf("CreateWindowEx : %v", err)
	}
	a.hwnd = h
	a.taskbarMsg, _, _ = procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(utf16("TaskbarCreated"))))
	return nil
}

func (a *app) handle(hwnd uintptr, m uint32, wParam, lParam uintptr) (uintptr, bool) {
	switch {
	case m == wmTrayIcon:
		switch uint32(lParam & 0xFFFF) {
		case wmContextMenu, ninSelect, ninKeySelect:
			a.showMenu()
		case ninBalloonUserClick:
			if a.balloonDir != "" {
				_ = shellExecute(a.hwnd, "open", a.balloonDir, "", swShow)
			}
		}
		return 0, true
	case m == wmInvoke:
		a.drain()
		return 0, true
	case m == wmTimer:
		switch wParam {
		case timerReload:
			a.reload()
		case timerSoon:
			procKillTimer.Call(a.hwnd, timerSoon)
			a.reload()
		case timerAnimate:
			a.frame++
			a.setIcon()
		}
		return 0, true
	case a.taskbarMsg != 0 && uintptr(m) == a.taskbarMsg:
		a.iconAdded = false
		a.addIcon()
		return 0, true
	case m == wmDestroy:
		procPostQuitMessage.Call(0)
		return 0, true
	}
	return 0, false
}

// invoke exécute f sur le thread de l'interface ; appelable depuis toute goroutine.
func (a *app) invoke(f func()) {
	a.mu.Lock()
	a.queue = append(a.queue, f)
	a.mu.Unlock()
	procPostMessageW.Call(a.hwnd, wmInvoke, 0, 0)
}

func (a *app) drain() {
	for {
		a.mu.Lock()
		if len(a.queue) == 0 {
			a.mu.Unlock()
			return
		}
		f := a.queue[0]
		a.queue = a.queue[1:]
		a.mu.Unlock()
		f() // peut ouvrir un dialogue modal : la file est relue ensuite
	}
}

// --- état --------------------------------------------------------------------

// reload relit configuration, état et suspension, puis met l'icône à jour.
func (a *app) reload() {
	if fi, err := os.Stat(a.opt.ConfigFile); err == nil && (a.cfg == nil && a.cfgErr == nil || !fi.ModTime().Equal(a.cfgMod)) {
		a.cfgMod = fi.ModTime()
		a.cfg, a.cfgErr = config.Load(a.opt.ConfigFile, config.LoadOptions{})
		if a.cfgErr != nil {
			a.log.Warn("configuration invalide", "err", a.cfgErr)
		}
	}
	st, stErr := state.Read(paths.StatusFile())
	if stErr != nil {
		a.log.Warn("status.json illisible", "err", stErr)
		st = a.status
	}
	susp, _ := state.ReadSuspension(paths.SuspendFile())

	a.mu.Lock()
	connected := a.peer != nil
	a.mu.Unlock()
	now := time.Now()
	in := Input{Status: st, Config: a.cfg, ConfigErr: a.cfgErr, Suspension: susp, User: a.user, Connected: connected, Now: now}
	v := Compute(in)

	toastOn := []string{"failure", "warning"}
	if a.cfg != nil {
		toastOn = a.cfg.Notify.ToastOn
	}
	if a.seenStatus {
		if t := FinishedToast(a.status, st, toastOn); t != nil {
			dir := ""
			if t.OpenLog && st != nil {
				dir = filepath.Join(paths.RunsDir(), st.RunID)
			}
			a.balloon(*t, dir)
		}
	}
	if t := LateToast(v, a.lastLate, now); t != nil && a.iconAdded {
		a.lastLate = now
		a.balloon(*t, "")
	}
	a.status, a.seenStatus, a.view = st, true, v

	if v.Running {
		procSetTimer.Call(a.hwnd, timerAnimate, 600, 0)
	} else {
		procKillTimer.Call(a.hwnd, timerAnimate)
		a.frame = 0
	}
	a.setIcon()
}

func (a *app) iconHandle() uintptr {
	key := [2]int{int(a.view.Icon), a.frame % 2}
	if a.view.Icon != IconRunning {
		key[1] = 0
	}
	if h, ok := a.icons[key]; ok {
		return h
	}
	h, err := hicon(a.view.Icon, key[1])
	if err != nil {
		a.log.Warn("icône", "err", err)
		return 0
	}
	a.icons[key] = h
	return h
}

func (a *app) nid() notifyIconData {
	return notifyIconData{Size: uint32(unsafe.Sizeof(notifyIconData{})), Wnd: a.hwnd, ID: 1}
}

func (a *app) addIcon() {
	if a.iconAdded {
		return
	}
	n := a.nid()
	n.Flags = nifMessage | nifIcon | nifTip | nifShowTip
	n.CallbackMessage = wmTrayIcon
	n.Icon = a.iconHandle()
	copyUTF16(n.Tip[:], a.view.Tooltip)
	if r, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&n))); r == 0 {
		// Barre des tâches pas encore prête (ouverture de session) : nouvel essai bientôt.
		procSetTimer.Call(a.hwnd, timerSoon, 2000, 0)
		return
	}
	n.Version = notifyIconV4
	procShellNotifyIconW.Call(nimSetVersion, uintptr(unsafe.Pointer(&n)))
	a.iconAdded = true
	a.currentIcon = n.Icon
	if t := LateToast(a.view, a.lastLate, time.Now()); t != nil {
		a.lastLate = time.Now()
		a.balloon(*t, "")
	}
}

func (a *app) setIcon() {
	if !a.iconAdded {
		a.addIcon()
		return
	}
	n := a.nid()
	n.Flags = nifIcon | nifTip | nifShowTip
	n.Icon = a.iconHandle()
	copyUTF16(n.Tip[:], a.view.Tooltip)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
	a.currentIcon = n.Icon
}

func (a *app) removeIcon() {
	n := a.nid()
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&n)))
	a.iconAdded = false
}

// balloon affiche une notification (toast sous Windows 10 et suivants).
func (a *app) balloon(t Toast, openDir string) {
	if !a.iconAdded {
		return
	}
	a.balloonDir = openDir
	n := a.nid()
	n.Flags = nifInfo
	copyUTF16(n.InfoTitle[:], t.Title)
	copyUTF16(n.Info[:], t.Text)
	n.InfoFlags = uint32(t.Level)
	if t.Level == LevelInfo {
		n.InfoFlags |= niifNoSound
	}
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

// --- menu --------------------------------------------------------------------

func (a *app) showMenu() {
	a.reload()
	v := a.view
	menu, _, _ := procCreatePopupMenu.Call()
	defer procDestroyMenu.Call(menu)
	add := func(h uintptr, flags uintptr, id int, text string) {
		procAppendMenuW.Call(h, flags, uintptr(id), uintptr(unsafe.Pointer(utf16(text))))
	}
	sep := func() { procAppendMenuW.Call(menu, mfSeparator, 0, 0) }

	add(menu, mfString|mfGrayed, 0, v.LastLine)
	if v.NextLine != "" {
		add(menu, mfString|mfGrayed, 0, v.NextLine)
	}
	sep()
	if v.ShowRunNow {
		flags := uintptr(mfString)
		if !v.RunNowEnabled {
			flags |= mfGrayed
		}
		add(menu, flags, cmdRunNow, "Sauvegarder maintenant")
	}
	if v.ShowCancel {
		add(menu, mfString, cmdCancel, "Annuler la sauvegarde")
	}
	if v.ShowRunNow { // mêmes comptes : ceux qui pilotent la sauvegarde
		sub, _, _ := procCreatePopupMenu.Call()
		add(sub, mfString, cmdSuspend1h, "Pendant 1 h")
		add(sub, mfString, cmdSuspendTomorrow, "Jusqu'à demain 08:00")
		if v.Suspended {
			add(sub, mfString, cmdResume, "Reprendre")
		}
		procAppendMenuW.Call(menu, mfPopup, sub, uintptr(unsafe.Pointer(utf16("Suspendre"))))
	}
	sep()
	add(menu, mfString, cmdOpenLogs, "Ouvrir les journaux")
	add(menu, mfString, cmdOpenConfig, "Ouvrir la configuration")
	add(menu, mfString, cmdCheckConfig, "Vérifier la configuration")
	add(menu, mfString, cmdAbout, "À propos")
	sep()
	add(menu, mfString, cmdQuit, "Quitter l'icône (la sauvegarde planifiée continue)")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(a.hwnd) // sinon le menu ne se ferme pas en cliquant ailleurs
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightBtn|tpmReturn, uintptr(pt.X), uintptr(pt.Y), 0, a.hwnd, 0)
	procPostMessageW.Call(a.hwnd, wmNull, 0, 0)
	a.command(int(cmd))
}

func (a *app) command(cmd int) {
	switch cmd {
	case cmdRunNow:
		a.runNow()
	case cmdCancel:
		if messageBox(a.hwnd, "Kura", "Annuler la sauvegarde en cours ?\nLa suivante reprendra là où celle-ci s'est arrêtée.",
			mbYesNo|mbIconWarning) != idYes {
			return
		}
		a.mu.Lock()
		p := a.peer
		a.mu.Unlock()
		if p == nil || p.Send(ipc.Msg{Type: ipc.TypeCancel}) != nil {
			messageBox(a.hwnd, "Kura", "La sauvegarde en cours ne répond pas à l'icône : annulation impossible d'ici.", mbOK|mbIconWarning)
			return
		}
		a.log.Info("annulation demandée")
	case cmdSuspend1h:
		a.elevated("suspend 1h")
	case cmdSuspendTomorrow:
		a.elevated("suspend tomorrow")
	case cmdResume:
		a.elevated("suspend off")
	case cmdOpenLogs:
		dir := paths.RunsDir()
		if a.status != nil {
			if d := filepath.Join(dir, a.status.RunID); dirExists(d) {
				dir = d
			}
		}
		if !dirExists(dir) {
			dir = paths.DataDir()
		}
		_ = shellExecute(a.hwnd, "open", dir, "", swShow)
	case cmdOpenConfig:
		_ = shellExecute(a.hwnd, "open", "notepad.exe", `"`+a.opt.ConfigFile+`"`, swShow)
	case cmdCheckConfig:
		if _, err := config.Load(a.opt.ConfigFile, config.LoadOptions{}); err != nil {
			messageBox(a.hwnd, "Kura – configuration", err.Error(), mbOK|mbIconError)
		} else {
			messageBox(a.hwnd, "Kura – configuration", "Configuration valide.\n\nVérification complète (sources, aws.exe, accès au bucket) :\nkura-agent validate, dans une console d'administration.", mbOK|mbIconInfo)
		}
	case cmdAbout:
		host, _ := os.Hostname()
		text := fmt.Sprintf("Kura agent %s (commit %s)\n\nServeur : %s\nSession : %s\nConfiguration : %s",
			a.opt.Version, a.opt.Commit, host, a.user, a.opt.ConfigFile)
		if a.cfg != nil {
			text += fmt.Sprintf("\nProfil : %s\nBucket : %s", a.cfg.Profile.Name, a.cfg.Profile.Bucket)
		}
		messageBox(a.hwnd, "À propos de Kura", text, mbOK|mbIconInfo)
	case cmdQuit:
		a.removeIcon()
		procDestroyWindow.Call(a.hwnd)
	}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// runNow déclenche la tâche planifiée : même compte, même journal, même verrou que
// la nuit. Le droit de la lancer est donné par install aux comptes de allow_run_now.
func (a *app) runNow() {
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "schtasks.exe"), "/Run", "/TN", schedtask.Name)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if err != nil {
		a.log.Warn("lancement de la tâche", "err", err, "sortie", strings.TrimSpace(string(out)))
		messageBox(a.hwnd, "Kura", "Lancement impossible de la tâche "+schedtask.Name+" :\n"+strings.TrimSpace(string(out)), mbOK|mbIconError)
		return
	}
	a.log.Info("sauvegarde lancée depuis l'icône")
	a.balloon(Toast{Title: "Sauvegarde lancée", Text: "L'icône indique l'avancement.", Level: LevelInfo}, "")
	procSetTimer.Call(a.hwnd, timerSoon, 3000, 0)
}

// elevated lance kura-agentw.exe avec élévation (invite UAC) : la suspension modifie
// C:\ProgramData\Kura, réservé aux administrateurs.
func (a *app) elevated(args string) {
	if err := shellExecute(a.hwnd, "runas", a.exe, args, swHide); err != nil {
		a.log.Warn("commande élevée", "args", args, "err", err)
		return
	}
	procSetTimer.Call(a.hwnd, timerSoon, 3000, 0)
}

// --- dialogue avec run -------------------------------------------------------

// serve attend les connexions de run sur le canal de la session, une à la fois.
func (a *app) serve() {
	l, err := ipc.Listen()
	if err != nil {
		a.log.Error("canal de l'icône indisponible : préavis et relance d'Outlook désactivés", "err", err)
		return
	}
	a.mu.Lock()
	a.listener = l
	a.mu.Unlock()
	for {
		conn, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return // icône qui quitte
		}
		if err != nil {
			a.log.Warn("connexion de run", "err", err)
			time.Sleep(time.Second)
			continue
		}
		ready := make(chan struct{})
		var p *ipc.Peer
		p = ipc.NewPeer(conn, func(m ipc.Msg) {
			<-ready
			a.onRunMsg(p, m)
		})
		a.mu.Lock()
		a.peer = p
		a.mu.Unlock()
		close(ready)
		a.invoke(a.reload)
		<-p.Done()
		a.mu.Lock()
		a.peer = nil
		a.mu.Unlock()
		a.invoke(a.reload)
	}
}

// onRunMsg est appelé depuis la goroutine de lecture : les requêtes longues partent
// dans leur propre goroutine pour que l'annulation et les événements passent.
func (a *app) onRunMsg(p *ipc.Peer, m ipc.Msg) {
	switch m.Type {
	case ipc.TypeHello, ipc.TypeStarted, ipc.TypeJob, ipc.TypeFinished:
		a.invoke(a.reload)
	case ipc.TypePromptClose:
		go func() {
			choice := a.prompt(p, m)
			if choice == "" {
				return // exécution partie entre-temps : rien à répondre
			}
			a.log.Info("préavis", "processus", m.Process, "choix", choice)
			_ = p.Reply(m, ipc.Msg{Type: ipc.TypeAnswer, Choice: choice})
		}()
	case ipc.TypeClose:
		go func() {
			left := closeApp(m.Process, time.Duration(m.GracefulS)*time.Second)
			a.log.Info("fermeture douce", "processus", m.Process, "restants", left)
			_ = p.Reply(m, ipc.Msg{Type: ipc.TypeClosed, Remaining: left})
		}()
	case ipc.TypeStart:
		go func() {
			err := startApp(m.Process, m.Path)
			r := ipc.Msg{Type: ipc.TypeStartedProc, OK: err == nil}
			if err != nil {
				r.Message = err.Error()
				a.log.Warn("relance", "processus", m.Process, "err", err)
			} else {
				a.log.Info("relance", "processus", m.Process)
			}
			_ = p.Reply(m, r)
		}()
	}
}

// prompt affiche le préavis sur le thread de l'interface et attend le choix. Si la
// fenêtre ne peut pas s'ouvrir, une notification prévient et le délai s'écoule.
func (a *app) prompt(p *ipc.Peer, m ipc.Msg) string {
	// Un seul préavis à la fois. Le préavis d'une exécution qui s'arrête (connexion
	// perdue) est fermé aussitôt : il ne retient pas celui de l'exécution suivante.
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	select {
	case <-p.Done():
		return "" // demande d'une exécution déjà terminée
	default:
	}
	a.mu.Lock()
	a.promptSeq++
	seq := a.promptSeq
	a.mu.Unlock()
	watch := make(chan struct{})
	defer close(watch)
	go func() {
		select {
		case <-p.Done():
			a.invoke(func() { abandonDialog(seq) })
		case <-watch:
		}
	}()

	type result struct {
		choice string
		err    error
	}
	ch := make(chan result, 1)
	a.invoke(func() {
		c, err := closeDialog(0, m, seq) // sans propriétaire : centrée sur l'écran, pas sur la fenêtre cachée
		ch <- result{c, err}
	})
	r := <-ch
	if r.err == nil {
		return r.choice
	}
	a.log.Warn("fenêtre de préavis indisponible", "err", r.err)
	a.invoke(func() {
		a.balloon(Toast{Title: displayName(m.Process) + " va être fermé",
			Text:  fmt.Sprintf("Fermeture dans %d min pour la sauvegarde : enregistrez votre travail.", (m.NoticeS+59)/60),
			Level: LevelWarning}, "")
	})
	select {
	case <-time.After(time.Duration(m.NoticeS) * time.Second):
	case <-p.Done():
		return ""
	}
	return ipc.ChoiceTimeout
}
