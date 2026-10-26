package ipc

import (
	"context"
	"errors"
	"io"
	"sync"
)

// Dialer ouvre le canal de l'icône d'une session ; une erreur signifie « pas d'icône ».
type Dialer func(session int) (io.ReadWriteCloser, error)

// ErrNoTray : aucune icône ne répond dans cette session.
var ErrNoTray = errors.New("pas d'icône dans cette session")

// Hub est le côté run : une connexion par session qui a une icône.
type Hub struct {
	dial     Dialer
	hello    Msg
	onCancel func(session int)

	mu    sync.Mutex
	peers map[int]*Peer
}

// NewHub prépare le côté run. hello est envoyé à chaque nouvelle connexion ;
// onCancel est appelé quand une icône demande l'annulation.
func NewHub(dial Dialer, hello Msg, onCancel func(session int)) *Hub {
	if onCancel == nil {
		onCancel = func(int) {}
	}
	hello.Type = TypeHello
	return &Hub{dial: dial, hello: hello, onCancel: onCancel, peers: map[int]*Peer{}}
}

// Connect tente de joindre l'icône de chaque session ; les échecs sont silencieux.
func (h *Hub) Connect(sessions []int) {
	for _, s := range sessions {
		_, _ = h.peer(s)
	}
}

// peer renvoie la connexion de la session, en la (r)ouvrant au besoin : une icône
// lancée après le début de run est jointe au moment où on en a besoin.
func (h *Hub) peer(session int) (*Peer, error) {
	h.mu.Lock()
	p := h.peers[session]
	h.mu.Unlock()
	if p != nil {
		select {
		case <-p.Done():
		default:
			return p, nil
		}
	}
	if h.dial == nil {
		return nil, ErrNoTray
	}
	conn, err := h.dial(session)
	if err != nil {
		return nil, ErrNoTray
	}
	p = NewPeer(conn, func(m Msg) {
		if m.Type == TypeCancel {
			h.onCancel(session)
		}
	})
	if err := p.Send(h.hello); err != nil {
		p.Close()
		return nil, ErrNoTray
	}
	h.mu.Lock()
	h.peers[session] = p
	h.mu.Unlock()
	return p, nil
}

// Has indique si une icône répond dans la session.
func (h *Hub) Has(session int) bool {
	_, err := h.peer(session)
	return err == nil
}

// Broadcast envoie un événement à toutes les icônes jointes.
func (h *Hub) Broadcast(m Msg) {
	h.mu.Lock()
	peers := make([]*Peer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.Unlock()
	for _, p := range peers {
		_ = p.Send(m)
	}
}

// Request envoie une requête à l'icône d'une session et attend sa réponse.
func (h *Hub) Request(ctx context.Context, session int, m Msg) (Msg, error) {
	p, err := h.peer(session)
	if err != nil {
		return Msg{}, err
	}
	return p.Request(ctx, m)
}

// Close ferme toutes les connexions.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s, p := range h.peers {
		p.Close()
		delete(h.peers, s)
	}
}
