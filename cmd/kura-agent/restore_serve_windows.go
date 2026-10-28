package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/windows"

	"github.com/E-C-G/kura-agent/internal/ipc"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/procs"
)

// winTrayUser : compte de l'icône, d'après le jeton de son processus (copié en jeton
// d'emprunt d'identité par ipc.DialRestore).
type winTrayUser struct {
	tok     windows.Token
	account string
	home    string
}

func newWinTrayUser(tok windows.Token) (*winTrayUser, error) {
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	name, domain, _, err := u.User.Sid.LookupAccount("")
	if err != nil {
		return nil, err
	}
	account := name
	if domain != "" {
		account = domain + `\` + name
	}
	home, _ := tok.GetUserProfileDirectory()
	return &winTrayUser{tok: tok, account: account, home: home}, nil
}

func (u *winTrayUser) Account() string { return u.account }
func (u *winTrayUser) Home() string    { return u.home }
func (u *winTrayUser) Close()          { u.tok.Close() }

func (u *winTrayUser) Impersonate() (func(), error) {
	if err := windows.SetThreadToken(nil, u.tok); err != nil {
		return nil, fmt.Errorf("emprunt de l'identité de %s : %w", u.account, err)
	}
	return func() { _ = windows.RevertToSelf() }, nil
}

func (u *winTrayUser) Do(f func() error) error {
	ch := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // jamais déverrouillé : le thread disparaît avec la goroutine
		undo, err := u.Impersonate()
		if err != nil {
			ch <- err
			return
		}
		defer undo()
		ch <- f()
	}()
	return <-ch
}

const (
	fileAddFile         = 0x0002
	fileAddSubdirectory = 0x0004
)

// CanWrite ouvre dir pour y ajouter fichiers et dossiers, au nom du compte : Windows
// fait le contrôle d'accès avec son jeton (ni SYSTEM ni privilège de sauvegarde).
func (u *winTrayUser) CanWrite(dir string) error {
	return u.Do(func() error { return canWriteHere(dir) })
}

// CanWriteAll : CanWrite pour chaque dossier, sur un seul thread au nom du compte.
func (u *winTrayUser) CanWriteAll(dirs []string) []error {
	out := make([]error, len(dirs))
	if err := u.Do(func() error {
		for i, d := range dirs {
			out[i] = canWriteHere(d)
		}
		return nil
	}); err != nil {
		for i := range out {
			out[i] = err
		}
	}
	return out
}

// canWriteHere : contrôle sur le thread courant, qui porte déjà l'identité du compte.
func canWriteHere(dir string) error {
	{
		p, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			return err
		}
		h, err := windows.CreateFile(p, fileAddFile|fileAddSubdirectory,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(h, &info); err != nil {
			return err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.New("lien symbolique ou jonction")
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return errors.New("pas un dossier")
		}
		return nil
	}
}

func realServeDeps() (serveDeps, func()) {
	_ = procs.EnableDebugPrivilege()
	logw := io.Writer(os.Stderr)
	var closeLog func()
	if err := os.MkdirAll(paths.LogsDir(), 0o755); err == nil {
		if f, err := os.OpenFile(filepath.Join(paths.LogsDir(), "restore-tray.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			logw, closeLog = f, func() { f.Close() }
		}
	}
	if closeLog == nil {
		closeLog = func() {}
	}
	return serveDeps{
		restoreDeps: realRestoreDeps(),
		sessions:    ipc.Sessions,
		dial: func(s int) (io.ReadWriteCloser, trayUser, error) {
			conn, err := ipc.DialRestore(s)
			if err != nil {
				return nil, nil, err
			}
			tc, ok := conn.(interface{ ServerToken() (windows.Token, error) })
			if !ok {
				conn.Close()
				return nil, nil, errors.New("jeton de l'icône indisponible")
			}
			tok, err := tc.ServerToken()
			if err != nil {
				conn.Close()
				return nil, nil, err
			}
			u, err := newWinTrayUser(tok)
			if err != nil {
				tok.Close()
				conn.Close()
				return nil, nil, err
			}
			return conn, u, nil
		},
		poll:   2 * time.Second,
		idle:   30 * time.Minute,
		linger: 90 * time.Second,
		logw:   logw,
	}, closeLog
}
