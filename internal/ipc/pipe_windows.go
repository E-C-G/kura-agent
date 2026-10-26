//go:build windows

package ipc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
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
	writeTimeout  = 10 * time.Second

	// Clé de l'utilisateur où l'icône publie le nom de son canal.
	regKey   = `Software\Kura`
	regValue = "Pipe"
)

// serverPrefix : seuls les exécutables Kura (y compris un kura-agentw.exe.old-…
// renommé par une mise à jour pendant qu'il tournait) peuvent servir le canal. Le
// dossier de l'exécutable, réservé aux administrateurs, fait la vraie garantie.
var serverPrefix = []string{"kura-agent"}

// Le nom du canal est aléatoire et publié dans HKCU de l'utilisateur de la session :
// un autre utilisateur ne peut ni le deviner pour le prendre avant l'icône, ni le
// remplacer (il n'a pas accès à cette ruche).
func pipePattern(session int) *regexp.Regexp {
	return regexp.MustCompile(`^Kura-` + strconv.Itoa(session) + `-[0-9a-f]{32}$`)
}

func newPipeName(session uint32) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("Kura-%d-%s", session, hex.EncodeToString(b[:])), nil
}

// conn lit par PeekNamedPipe : une lecture synchrone bloquée sur un canal empêcherait
// toute écriture sur le même handle (Windows sérialise les E/S synchrones d'un handle).
// On ne lit donc que ce qui est déjà arrivé, et on attend par petites pauses.
type conn struct {
	mu        sync.Mutex // handle et lecture
	wmu       sync.Mutex
	h         windows.Handle
	closed    bool
	gone      atomic.Bool // fermée : plus aucune écriture
	nowait    bool        // côté run : écritures non bloquantes, avec délai
	close     func(windows.Handle) error
	serverSID string // côté run : compte du processus qui sert le canal
}

// ServerSID renvoie le SID du compte de l'icône qui sert le canal (côté run).
func (c *conn) ServerSID() string { return c.serverSID }

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

// Write : côté run, le handle est en PIPE_NOWAIT : une icône qui ne lit plus (ou un
// canal au tampon nul) ne bloque jamais run, l'écriture abandonne après writeTimeout.
func (c *conn) Write(b []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	deadline := time.Now().Add(writeTimeout)
	written := 0
	for written < len(b) {
		if c.gone.Load() {
			return written, io.ErrClosedPipe
		}
		var n uint32
		if err := windows.WriteFile(c.h, b[written:], &n, nil); err != nil {
			return written, err
		}
		written += int(n)
		if n == 0 {
			if !c.nowait || time.Now().After(deadline) {
				return written, errors.New("canal saturé : l'icône ne lit plus")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return written, nil
}

func (c *conn) Close() error {
	c.gone.Store(true)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.close(c.h)
}

// Listener est le canal de l'icône, accessible au seul utilisateur de la session,
// à SYSTEM (la tâche) et aux Administrateurs.
type Listener struct {
	h    windows.Handle
	Name string
}

// Listen crée le canal de l'icône sous un nom aléatoire et le publie dans
// HKCU\Software\Kura (valeur Pipe), où run le lit.
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
	pipe, err := newPipeName(session)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(`\\.\pipe\` + pipe)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateNamedPipe(name,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 64<<10, 64<<10, 0, &sa)
	if err != nil {
		return nil, fmt.Errorf("canal %s : %w", pipe, err)
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, regKey, registry.SET_VALUE)
	if err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("publication du canal : %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(regValue, pipe); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("publication du canal : %w", err)
	}
	return &Listener{h: h, Name: pipe}, nil
}

// Close retire la publication et ferme le canal.
func (l *Listener) Close() error {
	if k, err := registry.OpenKey(registry.CURRENT_USER, regKey, registry.QUERY_VALUE|registry.SET_VALUE); err == nil {
		if v, _, err := k.GetStringValue(regValue); err == nil && v == l.Name {
			_ = k.DeleteValue(regValue)
		}
		k.Close()
	}
	return windows.CloseHandle(l.h)
}

// Accept attend la connexion de run. La connexion rendue, une fois fermée, libère le
// canal pour la suivante (le handle est gardé d'une exécution à l'autre).
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

// pipeOf lit le canal publié par l'icône de la session dans la ruche de son utilisateur.
func pipeOf(session int) (string, error) {
	user, err := SessionUser(session)
	if err != nil {
		return "", err
	}
	sid, _, _, err := windows.LookupSID("", user)
	if err != nil {
		return "", err
	}
	k, err := registry.OpenKey(registry.USERS, sid.String()+`\`+regKey, registry.QUERY_VALUE)
	if err != nil {
		return "", ErrNoTray
	}
	defer k.Close()
	pipe, _, err := k.GetStringValue(regValue)
	if err != nil || !pipePattern(session).MatchString(pipe) {
		return "", ErrNoTray
	}
	return pipe, nil
}

// Dial ouvre le canal de l'icône d'une session, après avoir vérifié que le serveur est
// bien une icône Kura de cette session, lancée depuis le dossier de l'exécutable courant.
// Le niveau SECURITY_IDENTIFICATION interdit à l'icône d'emprunter l'identité de run
// (administrateur). Il faut SeDebugPrivilege pour inspecter le processus de l'icône
// (procs.EnableDebugPrivilege).
func Dial(session int) (io.ReadWriteCloser, error) {
	pipe, err := pipeOf(session)
	if err != nil {
		return nil, err
	}
	return dialPipe(pipe, session)
}

func dialPipe(pipe string, session int) (io.ReadWriteCloser, error) {
	name, err := windows.UTF16PtrFromString(`\\.\pipe\` + pipe)
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
	sid, err := checkServer(h, session)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	mode := uint32(windows.PIPE_READMODE_BYTE | windows.PIPE_NOWAIT)
	if err := windows.SetNamedPipeHandleState(h, &mode, nil, nil); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return &conn{h: h, close: windows.CloseHandle, nowait: true, serverSID: sid}, nil
}

// checkServer vérifie session et exécutable du serveur, et renvoie le SID de son compte.
func checkServer(h windows.Handle, session int) (string, error) {
	var pid, sid uint32
	if err := windows.GetNamedPipeServerProcessId(h, &pid); err != nil {
		return "", err
	}
	if err := windows.ProcessIdToSessionId(pid, &sid); err != nil {
		return "", err
	}
	if int(sid) != session {
		return "", fmt.Errorf("canal de la session %d servi depuis la session %d : ignoré", session, sid)
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(p)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(p, 0, &buf[0], &size); err != nil {
		return "", err
	}
	server := windows.UTF16ToString(buf[:size])
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	base := strings.ToLower(filepath.Base(server))
	allowed := false
	for _, pre := range serverPrefix {
		allowed = allowed || strings.HasPrefix(base, pre)
	}
	if !strings.EqualFold(filepath.Dir(server), filepath.Dir(self)) || !allowed {
		return "", fmt.Errorf("canal servi par %s, pas par une icône Kura : ignoré", server)
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
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
