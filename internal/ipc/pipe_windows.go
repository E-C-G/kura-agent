//go:build windows

package ipc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	advapi32                        = windows.NewLazySystemDLL("advapi32.dll")
	procImpersonateNamedPipeClient  = advapi32.NewProc("ImpersonateNamedPipeClient")
)

const (
	wtsUserName   = 5
	wtsDomainName = 7
	pollEvery     = 50 * time.Millisecond
	writeTimeout  = 10 * time.Second

	// Clé de l'utilisateur où l'icône publie le nom de son canal.
	regKey   = `Software\Kura`
	regValue = "Pipe"
	// Canal de la fenêtre de restauration, publié seulement pendant qu'elle est
	// ouverte : la tâche \Kura\Restore s'y connecte.
	regRestoreValue = "RestorePipe"

	prefixMain    = "Kura"
	prefixRestore = "KuraRestore"
)

// serverPrefix : seuls les exécutables Kura (y compris un kura-agentw.exe.old-…
// renommé par une mise à jour pendant qu'il tournait) peuvent servir le canal. Le
// dossier de l'exécutable, réservé aux administrateurs, fait la vraie garantie.
var serverPrefix = []string{"kura-agent"}

// Le nom du canal est aléatoire et publié dans HKCU de l'utilisateur de la session :
// un autre utilisateur ne peut ni le deviner pour le prendre avant l'icône, ni le
// remplacer (il n'a pas accès à cette ruche).
func pipePattern(prefix string, session int) *regexp.Regexp {
	return regexp.MustCompile(`^` + prefix + `-` + strconv.Itoa(session) + `-[0-9a-f]{32}$`)
}

func newPipeName(prefix string, session uint32) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d-%s", prefix, session, hex.EncodeToString(b[:])), nil
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
	serverSID string        // côté run : compte du processus qui sert le canal
	serverTok windows.Token // côté run : jeton de ce processus, pris lors de sa vérification
}

// ServerToken (côté tâche) : jeton d'emprunt d'identité du compte de l'icône qui sert
// le canal, pris sur son processus au moment où Dial l'a vérifié. À fermer par l'appelant.
func (c *conn) ServerToken() (windows.Token, error) {
	if c.serverTok == 0 {
		return 0, errors.New("jeton de l'icône indisponible")
	}
	var dup windows.Token
	if err := windows.DuplicateTokenEx(c.serverTok, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE, nil,
		windows.SecurityImpersonation, windows.TokenImpersonation, &dup); err != nil {
		return 0, err
	}
	return dup, nil
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

// ClientPrivileged (côté icône) : le client du canal est SYSTEM ou un processus élevé
// (administrateur). Le jeton du client est lu par emprunt d'identité au niveau
// « identification » (celui que demande Dial) : rien n'est fait en son nom.
func (c *conn) ClientPrivileged() bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if r, _, _ := procImpersonateNamedPipeClient.Call(uintptr(c.h)); r == 0 {
		return false
	}
	defer windows.RevertToSelf()
	var tok windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &tok); err != nil {
		return false
	}
	defer tok.Close()
	if tok.IsElevated() {
		return true
	}
	u, err := tok.GetTokenUser()
	return err == nil && u.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}

// Listener est le canal de l'icône, accessible au seul utilisateur de la session,
// à SYSTEM (la tâche) et aux Administrateurs.
type Listener struct {
	h     windows.Handle
	Name  string
	value string // valeur de HKCU\Software\Kura qui le publie

	mu        sync.Mutex
	closed    bool
	accepting bool // un ConnectNamedPipe synchrone attend sur h
}

// Listen crée le canal de l'icône sous un nom aléatoire et le publie dans
// HKCU\Software\Kura (valeur Pipe), où run le lit.
func Listen() (*Listener, error) { return listen(prefixMain, regValue) }

// ListenRestore crée le canal de la fenêtre de restauration (valeur RestorePipe), où
// la tâche \Kura\Restore le lit. Close retire la publication.
func ListenRestore() (*Listener, error) { return listen(prefixRestore, regRestoreValue) }

func listen(prefix, value string) (*Listener, error) {
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
	pipe, err := newPipeName(prefix, session)
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
	if err := k.SetStringValue(value, pipe); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("publication du canal : %w", err)
	}
	return &Listener{h: h, Name: pipe, value: value}, nil
}

// Close retire la publication et ferme le canal.
//
// Fermer le handle pendant qu'un autre fil attend dans ConnectNamedPipe (synchrone)
// peut bloquer indéfiniment : Windows sérialise les opérations sur un handle
// synchrone, la fermeture attend la fin de l'attente. Close réveille donc d'abord
// Accept en se connectant lui-même au canal, puis ferme.
func (l *Listener) Close() error {
	if k, err := registry.OpenKey(registry.CURRENT_USER, regKey, registry.QUERY_VALUE|registry.SET_VALUE); err == nil {
		if v, _, err := k.GetStringValue(l.value); err == nil && v == l.Name {
			_ = k.DeleteValue(l.value)
		}
		k.Close()
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	if name, err := windows.UTF16PtrFromString(`\\.\pipe\` + l.Name); err == nil {
		for deadline := time.Now().Add(2 * time.Second); l.isAccepting() && time.Now().Before(deadline); {
			h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
			if err == nil {
				windows.CloseHandle(h)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return windows.CloseHandle(l.h)
}

func (l *Listener) isAccepting() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepting
}

// Accept attend la connexion de run. La connexion rendue, une fois fermée, libère le
// canal pour la suivante (le handle est gardé d'une exécution à l'autre).
func (l *Listener) Accept() (io.ReadWriteCloser, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	l.accepting = true
	l.mu.Unlock()
	err := windows.ConnectNamedPipe(l.h, nil)
	l.mu.Lock()
	l.accepting = false
	closed := l.closed
	l.mu.Unlock()
	if closed {
		// Réveillé par Close : la connexion est la sienne.
		_ = windows.DisconnectNamedPipe(l.h)
		return nil, net.ErrClosed
	}
	if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		return nil, err
	}
	return &conn{h: l.h, close: func(h windows.Handle) error {
		_ = windows.FlushFileBuffers(h)
		return windows.DisconnectNamedPipe(h)
	}}, nil
}

// pipeOf lit le canal publié par l'icône de la session dans la ruche de son utilisateur.
func pipeOf(session int) (string, error) { return published(session, regValue, prefixMain) }

func published(session int, value, prefix string) (string, error) {
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
	pipe, _, err := k.GetStringValue(value)
	if err != nil || !pipePattern(prefix, session).MatchString(pipe) {
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

// DialRestore ouvre le canal de la fenêtre de restauration d'une session, s'il est
// publié (fenêtre ouverte), avec les mêmes vérifications que Dial. La connexion rendue
// sait donner le jeton du compte de l'icône (ServerToken).
func DialRestore(session int) (io.ReadWriteCloser, error) {
	pipe, err := published(session, regRestoreValue, prefixRestore)
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
	sid, tok, err := checkServer(h, session)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	mode := uint32(windows.PIPE_READMODE_BYTE | windows.PIPE_NOWAIT)
	if err := windows.SetNamedPipeHandleState(h, &mode, nil, nil); err != nil {
		tok.Close()
		windows.CloseHandle(h)
		return nil, err
	}
	return &conn{h: h, nowait: true, serverSID: sid, serverTok: tok, close: func(h windows.Handle) error {
		tok.Close()
		return windows.CloseHandle(h)
	}}, nil
}

// checkServer vérifie session et exécutable du serveur, et renvoie le SID de son compte
// et une copie de son jeton (emprunt d'identité).
func checkServer(h windows.Handle, session int) (string, windows.Token, error) {
	var pid, sid uint32
	if err := windows.GetNamedPipeServerProcessId(h, &pid); err != nil {
		return "", 0, err
	}
	if err := windows.ProcessIdToSessionId(pid, &sid); err != nil {
		return "", 0, err
	}
	if int(sid) != session {
		return "", 0, fmt.Errorf("canal de la session %d servi depuis la session %d : ignoré", session, sid)
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", 0, err
	}
	defer windows.CloseHandle(p)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(p, 0, &buf[0], &size); err != nil {
		return "", 0, err
	}
	server := windows.UTF16ToString(buf[:size])
	self, err := os.Executable()
	if err != nil {
		return "", 0, err
	}
	base := strings.ToLower(filepath.Base(server))
	allowed := false
	for _, pre := range serverPrefix {
		allowed = allowed || strings.HasPrefix(base, pre)
	}
	if !strings.EqualFold(filepath.Dir(server), filepath.Dir(self)) || !allowed {
		return "", 0, fmt.Errorf("canal servi par %s, pas par une icône Kura : ignoré", server)
	}
	// Jeton pris sur le processus vérifié ci-dessus, par le même handle : il ne peut pas
	// appartenir à un autre processus qui aurait repris son numéro.
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &tok); err != nil {
		return "", 0, err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", 0, err
	}
	var dup windows.Token
	if err := windows.DuplicateTokenEx(tok, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE|windows.TOKEN_DUPLICATE, nil,
		windows.SecurityImpersonation, windows.TokenImpersonation, &dup); err != nil {
		return "", 0, err
	}
	return u.User.Sid.String(), dup, nil
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
