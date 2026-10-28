// Package ipc est le dialogue entre `run` (tâche planifiée, session 0) et l'icône
// `tray` de chaque session : un message JSON par ligne sur un canal nommé
// \\.\pipe\Kura-<n° de session>, ouvert par l'icône, auquel run se connecte.
//
// run envoie des événements (started, job, finished) et des requêtes (prompt_close,
// close, start) auxquelles l'icône répond ; l'icône peut envoyer cancel à tout moment.
// Une requête sans réponse dans son délai vaut absence d'icône : run applique alors
// le comportement par défaut (attente du préavis puis arrêt forcé).
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Types de message.
const (
	// run → icône
	TypeHello       = "hello"        // premier message de run : RunID, Version
	TypeStarted     = "started"      // début : RunID, Total, DryRun
	TypeJob         = "job"          // job en cours : Job, Index (1..Total), Total
	TypeFinished    = "finished"     // fin : Result, ExitCode, Message
	TypePromptClose = "prompt_close" // préavis : Process, NoticeS, PostponeS, PostponesLeft → TypeAnswer
	TypeClose       = "close"        // fermeture douce : Process, GracefulS → TypeClosed
	TypeStart       = "start"        // relance : Process, Path → TypeStartedProc

	// self-update → icônes
	TypeUpdateProgress = "update_progress" // étape en cours : Message
	TypeUpdateDone     = "update_done"     // fin : OK, Version (installée), Message
	TypeRestart        = "restart"         // relancer l'icône depuis son exécutable (mis à jour)

	// icône → run
	TypeAnswer      = "answer"       // réponse au préavis : Choice
	TypeClosed      = "closed"       // réponse à close : Remaining
	TypeStartedProc = "started_proc" // réponse à start : OK, Message
	TypeCancel      = "cancel"       // annulation demandée depuis le menu
)

// Choix de l'utilisateur au préavis.
const (
	ChoiceCloseNow = "close_now" // bouton « Fermer maintenant »
	ChoiceTimeout  = "timeout"   // compte à rebours écoulé
	ChoicePostpone = "postpone"  // bouton « Reporter »
)

// Msg est un message dans un sens ou dans l'autre ; seuls les champs utiles au type
// sont renseignés.
type Msg struct {
	Type    string `json:"type"`
	ID      int    `json:"id,omitempty"`       // numéro d'une requête
	ReplyTo int    `json:"reply_to,omitempty"` // numéro de la requête à laquelle on répond

	Version string `json:"version,omitempty"`
	RunID   string `json:"run_id,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Job     string `json:"job,omitempty"`
	Index   int    `json:"index,omitempty"`
	Total   int    `json:"total,omitempty"`

	Process       string `json:"process,omitempty"`
	Path          string `json:"path,omitempty"`
	NoticeS       int    `json:"notice_s,omitempty"`
	PostponeS     int    `json:"postpone_s,omitempty"`
	PostponesLeft int    `json:"postpones_left,omitempty"`
	GracefulS     int    `json:"graceful_s,omitempty"`

	Choice    string `json:"choice,omitempty"`
	Remaining int    `json:"remaining,omitempty"`
	OK        bool   `json:"ok,omitempty"`

	Result   string `json:"result,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// maxLine borne un message : rien de légitime n'approche cette taille.
const maxLine = 64 << 10

// ErrClosed : la connexion est fermée (icône quittée, run terminé).
var ErrClosed = errors.New("canal fermé")

// Peer est une extrémité de connexion. Les messages reçus qui répondent à une requête
// en cours (ReplyTo) sont rendus à Request ; les autres sont passés à onMsg, appelé
// depuis la goroutine de lecture.
type Peer struct {
	conn  io.ReadWriteCloser
	onMsg func(Msg)

	wmu sync.Mutex
	enc *json.Encoder

	mu      sync.Mutex
	nextID  int
	pending map[int]chan Msg
	done    chan struct{}
	err     error
	once    sync.Once
}

// NewPeer démarre la lecture de conn.
func NewPeer(conn io.ReadWriteCloser, onMsg func(Msg)) *Peer {
	if onMsg == nil {
		onMsg = func(Msg) {}
	}
	p := &Peer{conn: conn, onMsg: onMsg, enc: json.NewEncoder(conn),
		pending: map[int]chan Msg{}, done: make(chan struct{})}
	go p.read()
	return p
}

func (p *Peer) read() {
	sc := bufio.NewScanner(p.conn)
	sc.Buffer(make([]byte, 4096), maxLine)
	for sc.Scan() {
		var m Msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue // ligne illisible : ignorée, la connexion reste utilisable
		}
		if m.ReplyTo != 0 {
			p.mu.Lock()
			ch := p.pending[m.ReplyTo]
			delete(p.pending, m.ReplyTo)
			p.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		p.onMsg(m)
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	p.shutdown(err)
}

func (p *Peer) shutdown(err error) {
	p.once.Do(func() {
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		// Fermer (et, côté icône, déconnecter le canal) avant de signaler la fin : la
		// connexion suivante ne doit jamais trouver le canal encore occupé par celle-ci.
		_ = p.conn.Close()
		close(p.done)
	})
}

// Send envoie un message.
func (p *Peer) Send(m Msg) error {
	select {
	case <-p.done:
		return ErrClosed
	default:
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if err := p.enc.Encode(m); err != nil {
		p.shutdown(err)
		return fmt.Errorf("%w : %v", ErrClosed, err)
	}
	return nil
}

// Reply répond à la requête req.
func (p *Peer) Reply(req Msg, m Msg) error {
	m.ReplyTo = req.ID
	return p.Send(m)
}

// Request envoie une requête et attend sa réponse, au plus jusqu'à la fin de ctx.
func (p *Peer) Request(ctx context.Context, m Msg) (Msg, error) {
	ch := make(chan Msg, 1)
	p.mu.Lock()
	p.nextID++
	m.ID = p.nextID
	p.pending[m.ID] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, m.ID)
		p.mu.Unlock()
	}()
	if err := p.Send(m); err != nil {
		return Msg{}, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-p.done:
		return Msg{}, ErrClosed
	case <-ctx.Done():
		return Msg{}, ctx.Err()
	}
}

// Done est fermé quand la connexion est perdue.
func (p *Peer) Done() <-chan struct{} { return p.done }

// Close ferme la connexion.
func (p *Peer) Close() error {
	p.shutdown(ErrClosed)
	return nil
}
