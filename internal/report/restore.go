package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// RestoreInfo résume une restauration pour le bilan.
type RestoreInfo struct {
	Account  string     `json:"account"`      // compte qui a restauré
	Paths    []string   `json:"paths"`        // chemins d'origine (ou les sources, --all)
	To       string     `json:"to"`           // destination
	At       *time.Time `json:"at,omitempty"` // date de l'état ; absent : dernière sauvegarde
	Files    int        `json:"files"`        // restaurés
	Bytes    int64      `json:"bytes"`        // volume restauré
	Skipped  int        `json:"skipped"`      // déjà restaurés (reprise)
	Ignored  int        `json:"ignored"`      // noms refusés, jamais écrits
	Failed   int        `json:"failed"`       // en échec
	Result   string     `json:"result"`       // ok | partial | interrupted
	ExitCode int        `json:"exit_code"`
	Log      string     `json:"log,omitempty"` // journal sur le poste
	Started  time.Time  `json:"started_at"`
	Ended    time.Time  `json:"ended_at"`
	Message  string     `json:"message,omitempty"`
}

// RestorePayload : message envoyé au webhook après une restauration.
type RestorePayload struct {
	Event        string    `json:"event"` // restore_finished
	Type         string    `json:"type"`  // restore
	Host         string    `json:"host"`
	Profile      string    `json:"profile"`
	Bucket       string    `json:"bucket"`
	AgentVersion string    `json:"agent_version"`
	SentAt       time.Time `json:"sent_at"`
	RestoreInfo
}

// RestoreSummary : texte court (journal du check healthchecks).
func RestoreSummary(r RestoreInfo) string {
	var b strings.Builder
	when := "dernière sauvegarde"
	if r.At != nil {
		when = r.At.Local().Format("02/01/2006 15:04")
	}
	fmt.Fprintf(&b, "kura-agent restore par %s : %s (code %d)\n", r.Account, r.Result, r.ExitCode)
	fmt.Fprintf(&b, "%s, état : %s → %s\n", strings.Join(r.Paths, ", "), when, r.To)
	fmt.Fprintf(&b, "%d fichier(s) restauré(s), %d octet(s)", r.Files, r.Bytes)
	if r.Skipped > 0 {
		fmt.Fprintf(&b, ", %d déjà restauré(s)", r.Skipped)
	}
	if r.Ignored > 0 {
		fmt.Fprintf(&b, ", %d nom(s) refusé(s)", r.Ignored)
	}
	if r.Failed > 0 {
		fmt.Fprintf(&b, ", %d en échec", r.Failed)
	}
	b.WriteString("\n")
	if r.Message != "" {
		b.WriteString(r.Message + "\n")
	}
	return b.String()
}

// Restore signale une restauration sans toucher à l'état de la sauvegarde : healthchecks
// reçoit une ligne sur <url>/log (journal du check, ni succès ni échec) ; le webhook un
// message type restore. Pas de file d'attente : un bilan de restauration non remis est
// seulement signalé.
func Restore(ctx context.Context, r *config.Report, store creds.Store, o Options, info RestoreInfo) error {
	rep, err := New(r, store, o)
	if err != nil || rep == nil {
		return err
	}
	switch x := rep.(type) {
	case *healthchecks:
		body := RestoreSummary(info)
		if len(body) > bodyLimit {
			body = body[:bodyLimit]
		}
		return send(ctx, x.opt, x.endpoint("/log"), []byte(body), http.Header{"Content-Type": {"text/plain; charset=utf-8"}})
	case *webhook:
		p := RestorePayload{Event: "restore_finished", Type: "restore", Host: x.opt.Host, Profile: x.opt.Profile,
			Bucket: x.opt.Bucket, AgentVersion: x.opt.Version, RestoreInfo: info}
		if p.Paths == nil {
			p.Paths = []string{}
		}
		return x.deliverJSON(ctx, func(sent time.Time) any { p.SentAt = sent; return p })
	}
	return nil
}

// deliverJSON envoie un message JSON signé comme les bilans (jeton ou HMAC).
func (w *webhook) deliverJSON(ctx context.Context, build func(sent time.Time) any) error {
	sent := w.opt.Now().UTC()
	body, err := json.Marshal(build(sent))
	if err != nil {
		return permanentError{err}
	}
	return send(ctx, w.opt, w.url, body, w.headers(sent, body))
}
