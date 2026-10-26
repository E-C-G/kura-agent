//go:build windows

package ipc

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Vrai canal nommé : l'icône (Listen/Accept) et run (Dial) dans le même processus.
func TestNamedPipeRoundTrip(t *testing.T) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	saved := serverPrefix
	serverPrefix = append(serverPrefix, strings.ToLower(filepath.Base(self)))
	defer func() { serverPrefix = saved }()
	l, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	published, err := pipeOfCurrentUser()
	if err != nil || published != l.Name {
		t.Fatalf("canal publié %q, %v ; attendu %q", published, err, l.Name)
	}
	served := make(chan int, 4)
	go func() {
		for i := 0; ; i++ {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			var p *Peer
			ready := make(chan struct{})
			p = NewPeer(conn, func(m Msg) {
				<-ready
				if m.Type == TypePromptClose {
					p.Reply(m, Msg{Type: TypeAnswer, Choice: ChoiceCloseNow})
				}
			})
			close(ready)
			served <- i
			<-p.Done()
		}
	}()

	for round := 0; round < 2; round++ { // deux exécutions successives sur le même canal
		hub := NewHub(func(s int) (io.ReadWriteCloser, error) { return dialPipe(l.Name, s) }, Msg{RunID: "test"}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ans, err := hub.Request(ctx, int(session), Msg{Type: TypePromptClose, Process: "outlook.exe", NoticeS: 1})
		cancel()
		if err != nil || ans.Choice != ChoiceCloseNow {
			t.Fatalf("tour %d : réponse %+v, %v", round, ans, err)
		}
		hub.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Fatalf("tour %d : connexion non servie", round)
		}
	}
	if _, err := dialPipe(l.Name, int(session)+1000); err == nil {
		t.Fatal("canal accepté pour une autre session que celle de son serveur")
	}
}

func pipeOfCurrentUser() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, regKey, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(regValue)
	return v, err
}
