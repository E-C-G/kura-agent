//go:build windows

package ipc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Vrai canal nommé : l'icône (Listen/Accept) et run (Dial) dans le même processus.
func TestNamedPipeRoundTrip(t *testing.T) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	serverNames[strings.ToLower(filepath.Base(self))] = true
	defer delete(serverNames, strings.ToLower(filepath.Base(self)))
	l, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(); err == nil {
		t.Fatal("second canal du même nom accepté")
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
		hub := NewHub(Dial, Msg{RunID: "test"}, nil)
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
	if _, err := Dial(int(session) + 1000); err == nil {
		t.Fatal("canal d'une session inexistante ouvert")
	}
}
