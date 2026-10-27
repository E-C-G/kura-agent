package report

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/state"
)

// webhook envoie en POST le bilan JSON de chaque exécution réelle.
type webhook struct {
	url   string
	token string
	hmac  bool
	opt   Options
}

// Payload est le bilan envoyé au webhook (format documenté dans le README).
type Payload struct {
	Event        string     `json:"event"` // run_finished
	Host         string     `json:"host"`
	Profile      string     `json:"profile"`
	Bucket       string     `json:"bucket"`
	RunID        string     `json:"run_id"`
	Result       string     `json:"result"` // ok | warning | error | guard | cancelled
	Success      bool       `json:"success"`
	ExitCode     int        `json:"exit_code"`
	Partial      bool       `json:"partial"` // lancée avec --job : tous les jobs n'ont pas tourné
	Message      string     `json:"message,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	LastSuccess  *time.Time `json:"last_success,omitempty"`
	AgentVersion string     `json:"agent_version"`
	SentAt       time.Time  `json:"sent_at"`
	Jobs         []JobPart  `json:"jobs"`
}

// JobPart résume un job dans le bilan.
type JobPart struct {
	ID       string `json:"id"`
	Result   string `json:"result"`
	Uploaded int    `json:"uploaded"`
	Deleted  int    `json:"deleted"`
	Skipped  int    `json:"skipped,omitempty"`
	ExitCode int    `json:"exit_code"`
	Message  string `json:"message,omitempty"`
}

func (w *webhook) Name() string { return "webhook" }

func (w *webhook) Start(context.Context, state.Status, bool) error { return nil }

func (w *webhook) Finish(ctx context.Context, st state.Status, full bool) error {
	p := Payload{
		Event: "run_finished", Host: w.opt.Host, Profile: w.opt.Profile, Bucket: w.opt.Bucket,
		RunID: st.RunID, Result: st.Result, Success: full && Succeeded(st), ExitCode: st.ExitCode,
		Partial: !full, Message: st.Message, StartedAt: st.StartedAt, EndedAt: st.EndedAt,
		LastSuccess: st.LastSuccess, AgentVersion: w.opt.Version, Jobs: []JobPart{},
	}
	for _, j := range st.Jobs {
		p.Jobs = append(p.Jobs, JobPart{ID: j.ID, Result: j.Result, Uploaded: j.Uploaded,
			Deleted: j.Deleted, Skipped: j.Skipped, ExitCode: j.ExitCode, Message: j.Message})
	}

	// Bilans restés en attente d'abord, dans l'ordre, pour que le broker les voie tous.
	var errs []error
	if err := w.flushPending(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := w.deliver(ctx, p); err != nil {
		if qerr := w.queue(p); qerr != nil {
			errs = append(errs, fmt.Errorf("bilan non sauvegardé pour renvoi : %w", qerr))
		}
		errs = append(errs, fmt.Errorf("bilan gardé pour la prochaine exécution : %w", err))
	}
	return errors.Join(errs...)
}

func (w *webhook) deliver(ctx context.Context, p Payload) error {
	p.SentAt = w.opt.Now().UTC()
	body, err := json.Marshal(p)
	if err != nil {
		return permanentError{err}
	}
	h := http.Header{"Content-Type": {"application/json"}}
	if w.token != "" {
		if w.hmac {
			// La signature suffit : le jeton lui-même ne voyage pas.
			ts := strconv.FormatInt(p.SentAt.Unix(), 10)
			h.Set("X-Kura-Timestamp", ts)
			h.Set("X-Kura-Signature", "sha256="+Sign(w.token, ts, body))
		} else {
			h.Set("X-Kura-Token", w.token)
		}
	}
	return send(ctx, w.opt, w.url, body, h)
}

// Sign calcule la signature HMAC-SHA256 de « <timestamp>.<corps> » avec le jeton.
// Le broker recalcule la même valeur pour vérifier l'origine et l'intégrité du bilan ;
// l'horodatage inclus permet de refuser un bilan rejoué.
func Sign(token, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// pendingLimit : au-delà, les bilans les plus anciens sont abandonnés.
const pendingLimit = 20

func (w *webhook) queue(p Payload) error {
	if w.opt.PendingDir == "" {
		return errors.New("aucun dossier d'attente")
	}
	if err := os.MkdirAll(w.opt.PendingDir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	name := filepath.Join(w.opt.PendingDir, safeName(p.RunID)+".json")
	if err := os.WriteFile(name, data, 0o644); err != nil {
		return err
	}
	files := w.pendingFiles()
	for len(files) > pendingLimit {
		os.Remove(files[0])
		files = files[1:]
	}
	return nil
}

func (w *webhook) pendingFiles() []string {
	if w.opt.PendingDir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(w.opt.PendingDir, "*.json"))
	sort.Strings(files) // run_id horodaté : ordre chronologique
	return files
}

// flushPending renvoie les bilans en attente ; s'arrête au premier échec.
func (w *webhook) flushPending(ctx context.Context) error {
	for _, f := range w.pendingFiles() {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var p Payload
		if err := json.Unmarshal(data, &p); err != nil {
			os.Remove(f) // fichier corrompu : inutile de le garder
			continue
		}
		if err := w.deliver(ctx, p); err != nil {
			return fmt.Errorf("bilan en attente %s non renvoyé : %w", p.RunID, err)
		}
		os.Remove(f)
	}
	return nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
