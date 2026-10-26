package ipc

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeTray sert une connexion comme le ferait l'icône.
func fakeTray(t *testing.T, conn net.Conn, handle func(p *Peer, m Msg)) *Peer {
	t.Helper()
	var p *Peer
	ready := make(chan struct{})
	p = NewPeer(conn, func(m Msg) {
		<-ready
		handle(p, m)
	})
	close(ready)
	return p
}

func TestRequestReply(t *testing.T) {
	a, b := net.Pipe()
	var got []Msg
	var mu sync.Mutex
	tray := fakeTray(t, b, func(p *Peer, m Msg) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		if m.Type == TypePromptClose {
			p.Reply(m, Msg{Type: TypeAnswer, Choice: ChoicePostpone})
		}
	})
	defer tray.Close()
	hub := NewHub(func(s int) (io.ReadWriteCloser, error) {
		if s != 2 {
			return nil, errors.New("absent")
		}
		return a, nil
	}, Msg{RunID: "r1"}, nil)
	defer hub.Close()

	if hub.Has(3) {
		t.Fatal("icône inexistante vue")
	}
	ans, err := hub.Request(context.Background(), 2, Msg{Type: TypePromptClose, Process: "outlook.exe", NoticeS: 120})
	if err != nil || ans.Choice != ChoicePostpone {
		t.Fatalf("réponse %+v, %v", ans, err)
	}
	hub.Broadcast(Msg{Type: TypeFinished, Result: "ok"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0].Type != TypeHello || got[0].RunID != "r1" || got[2].Type != TypeFinished {
		t.Fatalf("messages reçus par l'icône : %+v", got)
	}
}

func TestCancelFromTray(t *testing.T) {
	a, b := net.Pipe()
	cancelled := make(chan int, 1)
	hub := NewHub(func(int) (io.ReadWriteCloser, error) { return a, nil }, Msg{}, func(s int) { cancelled <- s })
	defer hub.Close()
	tray := fakeTray(t, b, func(*Peer, Msg) {})
	defer tray.Close()
	hub.Connect([]int{5})
	if err := tray.Send(Msg{Type: TypeCancel}); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-cancelled:
		if s != 5 {
			t.Fatalf("annulation de la session %d", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("annulation non reçue")
	}
}

func TestRequestTimeoutAndLostTray(t *testing.T) {
	a, b := net.Pipe()
	tray := fakeTray(t, b, func(*Peer, Msg) {}) // ne répond jamais
	hub := NewHub(func(int) (io.ReadWriteCloser, error) { return a, nil }, Msg{}, nil)
	defer hub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := hub.Request(ctx, 1, Msg{Type: TypeClose}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("délai attendu, reçu %v", err)
	}
	// L'icône quitte pendant une requête : erreur immédiate, pas d'attente du délai.
	go func() { time.Sleep(50 * time.Millisecond); tray.Close() }()
	start := time.Now()
	if _, err := hub.Request(context.Background(), 1, Msg{Type: TypeClose}); err == nil {
		t.Fatal("requête réussie sans icône")
	}
	if time.Since(start) > time.Second {
		t.Fatal("perte de l'icône détectée trop tard")
	}
	// Plus de connexion, et le dialer ne rend plus rien d'utilisable : pas d'icône.
	hub.dial = func(int) (io.ReadWriteCloser, error) { return nil, errors.New("absent") }
	if hub.Has(1) {
		t.Fatal("icône quittée encore vue")
	}
}

func TestReconnectAfterTrayRestart(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	hub := NewHub(func(int) (io.ReadWriteCloser, error) {
		a, b := net.Pipe()
		mu.Lock()
		conns++
		mu.Unlock()
		NewPeer(b, nil)
		return a, nil
	}, Msg{}, nil)
	defer hub.Close()
	if !hub.Has(1) || !hub.Has(1) {
		t.Fatal("icône non jointe")
	}
	mu.Lock()
	if conns != 1 {
		t.Fatalf("%d connexions pour une icône vivante", conns)
	}
	mu.Unlock()
	hub.mu.Lock()
	hub.peers[1].Close()
	hub.mu.Unlock()
	if !hub.Has(1) {
		t.Fatal("pas de reconnexion")
	}
	mu.Lock()
	defer mu.Unlock()
	if conns != 2 {
		t.Fatalf("%d connexions, 2 attendues", conns)
	}
}

func TestGarbageLineIsIgnored(t *testing.T) {
	a, b := net.Pipe()
	got := make(chan Msg, 1)
	NewPeer(b, func(m Msg) { got <- m })
	go func() {
		a.Write([]byte("pas du json\n"))
		a.Write([]byte(`{"type":"cancel"}` + "\n"))
	}()
	select {
	case m := <-got:
		if m.Type != TypeCancel {
			t.Fatalf("message %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("message valide perdu après une ligne illisible")
	}
}
