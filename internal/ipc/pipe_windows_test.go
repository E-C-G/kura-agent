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

// Fermer le canal pendant qu'Accept attend ne doit pas bloquer (l'icône qui quitte).
func TestCloseWhileAccepting(t *testing.T) {
	self, _ := os.Executable()
	saved := serverPrefix
	serverPrefix = append(serverPrefix, strings.ToLower(filepath.Base(self)))
	defer func() { serverPrefix = saved }()
	for i := 0; i < 5; i++ {
		l, err := Listen()
		if err != nil {
			t.Fatal(err)
		}
		accepted := make(chan error, 1)
		go func() {
			_, err := l.Accept()
			accepted <- err
		}()
		for !l.isAccepting() {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond) // laisse ConnectNamedPipe commencer son attente
		closed := make(chan error, 1)
		go func() { closed <- l.Close() }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("Close bloqué pendant qu'Accept attend")
		}
		select {
		case err := <-accepted:
			if err == nil {
				t.Fatal("Accept a rendu une connexion après Close")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Accept toujours en attente après Close")
		}
	}
}

// L'icône reconnaît un client élevé (self-update lancé par l'invite UAC) ou SYSTEM :
// sur le runner de CI, le processus de test est élevé ; ailleurs, le résultat suit
// l'élévation du processus.
func TestClientPrivileged(t *testing.T) {
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
	got := make(chan bool, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			got <- false
			return
		}
		var p *Peer
		p = NewPeer(conn, func(m Msg) {
			if m.Type == TypeRestart {
				got <- conn.(interface{ ClientPrivileged() bool }).ClientPrivileged()
				p.Close()
			}
		})
	}()
	hub := NewHub(func(s int) (io.ReadWriteCloser, error) { return dialPipe(l.Name, s) }, Msg{}, nil)
	defer hub.Close()
	hub.Connect([]int{int(session)})
	hub.Broadcast(Msg{Type: TypeRestart})
	want := windows.GetCurrentProcessToken().IsElevated()
	select {
	case v := <-got:
		if v != want {
			t.Fatalf("client privilégié : %v, attendu %v (processus élevé : %v)", v, want, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("message non reçu")
	}
}
