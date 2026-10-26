//go:build windows

package ipc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                        = windows.NewLazySystemDLL("kernel32.dll")
	procPeekNamedPipe               = kernel32.NewProc("PeekNamedPipe")
	procWaitNamedPipeW              = kernel32.NewProc("WaitNamedPipeW")
	wtsapi32                        = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformationW = wtsapi32.NewProc("WTSQuerySessionInformationW")
)

const (
	wtsUserName   = 5
	wtsDomainName = 7
	pollEvery     = 50 * time.Millisecond
)

// serverNames : exécutables admis à servir le canal (en minuscules). Les tests y
// ajoutent leur propre binaire.
var serverNames = map[string]bool{"kura-agent.exe": true, "kura-agentw.exe": true}

// PipeName est le canal de l'icône d'une session.
func PipeName(session int) string { return `\\.\pipe\Kura-` + strconv.Itoa(session) }

// conn lit par PeekNamedPipe : une lecture synchrone bloquée sur un canal empêcherait
// toute écriture sur le même handle (Windows sérialise les E/S synchrones d'un handle).
// On ne lit donc que ce qui est déjà arrivé, et on attend par petites pauses.
type conn struct {
	mu     sync.Mutex // handle et lecture
	wmu    sync.Mutex
	h      windows.Handle
	closed bool
	close  func(windows.Handle) error
}

func (c *conn) Read(b []byte) (int, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return 0, io.EOF
		}
		var avail uint32
		r, _, err := procPeekNamedPipe.Call(uintptr(c.h), 0, 0, 0, uintptr(unsafe.Pointer(&avail)), 0)
		if r == 0 {
			c.mu.Unlock()
			if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) {
				return 0, io.EOF
			}
			return 0, err
		}
		if avail > 0 {
			n := uint32(len(b))
			if avail < n {
				n = avail
			}
			var done uint32
			err := windows.ReadFile(c.h, b[:n], &done, nil)
			c.mu.Unlock()
			if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
				return int(done), err
			}
			return int(done), nil
		}
		c.mu.Unlock()
		time.Sleep(pollEvery)
	}
}

func (c *conn) Write(b []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	written := 0
	for written < len(b) {
		var n uint32
		if err := windows.WriteFile(c.h, b[written:], &n, nil); err != nil {
			return written, err
		}
		written += int(n)
	}
	return written, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.close(c.h)
}

// Listener est le canal de l'icône, accessible au seul utilisateur de la session,
// à SYSTEM et aux Administrateurs (dont svc-kura).
type Listener struct {
	h windows.Handle
}

// Listen crée le canal de la session courante. Il échoue si le nom est déjà pris
// (autre icône, ou processus qui usurperait le canal).
func Listen() (*Listener, error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(PipeName(int(session)))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateNamedPipe(name,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 64<<10, 64<<10, 0, &sa)
	if err != nil {
		return nil, fmt.Errorf("canal %s : %w", PipeName(int(session)), err)
	}
	return &Listener{h: h}, nil
}

// Accept attend la connexion de run. La connexion rendue, une fois fermée, libère le
// canal pour la suivante (le handle est gardé : aucun autre processus ne peut
// prendre le nom entre deux exécutions).
func (l *Listener) Accept() (io.ReadWriteCloser, error) {
	err := windows.ConnectNamedPipe(l.h, nil)
	if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		return nil, err
	}
	return &conn{h: l.h, close: func(h windows.Handle) error {
		_ = windows.FlushFileBuffers(h)
		return windows.DisconnectNamedPipe(h)
	}}, nil
}

// Dial ouvre le canal de l'icône d'une session, après avoir vérifié que le serveur est
// bien une icône Kura de cette session : même dossier que l'exécutable courant.
// Le niveau SECURITY_IDENTIFICATION interdit à l'icône d'emprunter l'identité de run
// (administrateur).
func Dial(session int) (io.ReadWriteCloser, error) {
	name, err := windows.UTF16PtrFromString(PipeName(session))
	if err != nil {
		return nil, err
	}
	open := func() (windows.Handle, error) {
		return windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	}
	h, err := open()
	if errors.Is(err, windows.ERROR_PIPE_BUSY) {
		// L'icône libère encore la connexion précédente : on attend qu'elle se remette
		// à l'écoute, 2 s au plus.
		procWaitNamedPipeW.Call(uintptr(unsafe.Pointer(name)), 2000)
		h, err = open()
	}
	if err != nil {
		return nil, err
	}
	if err := checkServer(h, session); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return &conn{h: h, close: windows.CloseHandle}, nil
}

func checkServer(h windows.Handle, session int) error {
	var pid, sid uint32
	if err := windows.GetNamedPipeServerProcessId(h, &pid); err != nil {
		return err
	}
	if err := windows.ProcessIdToSessionId(pid, &sid); err != nil {
		return err
	}
	if int(sid) != session {
		return fmt.Errorf("canal de la session %d servi depuis la session %d : ignoré", session, sid)
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(p)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(p, 0, &buf[0], &size); err != nil {
		return err
	}
	server := windows.UTF16ToString(buf[:size])
	self, err := os.Executable()
	if err != nil {
		return err
	}
	base := strings.ToLower(filepath.Base(server))
	if !strings.EqualFold(filepath.Dir(server), filepath.Dir(self)) || !serverNames[base] {
		return fmt.Errorf("canal servi par %s, pas par une icône Kura : ignoré", server)
	}
	return nil
}

// Sessions liste les sessions Windows ouvertes (hors session 0 des services).
func Sessions() ([]int, error) {
	var info *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &info, &count); err != nil {
		return nil, err
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(info)))
	var out []int
	for _, s := range unsafe.Slice(info, count) {
		if s.SessionID != 0 && (s.State == windows.WTSActive || s.State == windows.WTSDisconnected) {
			out = append(out, int(s.SessionID))
		}
	}
	return out, nil
}

// SessionUser renvoie le compte (DOMAINE\nom) ouvert dans une session.
func SessionUser(session int) (string, error) {
	query := func(class uintptr) (string, error) {
		var buf *uint16
		var n uint32
		r, _, err := procWTSQuerySessionInformationW.Call(0, uintptr(session), class,
			uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
		if r == 0 {
			return "", err
		}
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
		return windows.UTF16PtrToString(buf), nil
	}
	user, err := query(wtsUserName)
	if err != nil || user == "" {
		return "", fmt.Errorf("session %d sans utilisateur", session)
	}
	if domain, err := query(wtsDomainName); err == nil && domain != "" {
		return domain + `\` + user, nil
	}
	return user, nil
}
