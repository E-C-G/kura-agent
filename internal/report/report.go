// Package report envoie le bilan de chaque exécution à un service de supervision :
// healthchecks.io (ou instance auto-hébergée) ou un webhook générique (Make, n8n…).
//
// L'envoi ne bloque jamais une sauvegarde : une destination injoignable est
// journalisée, et pour un webhook le bilan est gardé puis renvoyé la fois suivante.
package report

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Reporter reçoit le début et la fin d'une exécution réelle (jamais d'une simulation).
type Reporter interface {
	// Start signale le début de l'exécution. full : tous les jobs configurés sont
	// sélectionnés (pas d'exécution partielle --job).
	Start(ctx context.Context, st state.Status, full bool) error
	// Finish envoie le bilan. full indique que tous les jobs configurés étaient
	// sélectionnés (pas d'exécution partielle --job).
	Finish(ctx context.Context, st state.Status, full bool) error
	// Name décrit la destination pour les journaux (sans secret).
	Name() string
}

// Options règle l'envoi ; les valeurs nulles prennent des défauts raisonnables.
type Options struct {
	Version    string // version de l'agent, pour User-Agent et le bilan
	Host       string // nom de la machine
	Profile    string // profile.name de la configuration
	Bucket     string // profile.bucket
	PendingDir string // bilans webhook non envoyés, renvoyés la fois suivante
	Client     *http.Client
	Sleep      func(ctx context.Context, d time.Duration) error
	Backoff    []time.Duration // attente avant chaque nouvelle tentative ; défaut 5 s puis 15 s
	Now        func() time.Time
}

func (o *Options) defaults() {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if o.Backoff == nil {
		o.Backoff = []time.Duration{5 * time.Second, 15 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Version == "" {
		o.Version = "dev"
	}
}

// New construit le Reporter de la configuration ; nil, nil s'il n'y a pas de destination.
// Les références credential:<nom> sont résolues ici, une fois par exécution.
func New(r *config.Report, store creds.Store, o Options) (Reporter, error) {
	if r == nil {
		return nil, nil
	}
	o.defaults()
	url, err := store.Resolve(r.URL)
	if err != nil {
		return nil, fmt.Errorf("notify.report.url : %w", err)
	}
	url = strings.TrimRight(url, "/")
	switch r.Type {
	case config.ReportHealthchecks:
		rid, err := newUUID()
		if err != nil {
			return nil, err
		}
		return &healthchecks{url: url, rid: rid, opt: o}, nil
	case config.ReportWebhook:
		token := ""
		if r.Token != "" {
			if token, err = store.Resolve(r.Token); err != nil {
				return nil, fmt.Errorf("notify.report.token : %w", err)
			}
		}
		return &webhook{url: url, token: token, hmac: r.HMAC, opt: o}, nil
	default:
		return nil, fmt.Errorf("notify.report.type %q inconnu", r.Type)
	}
}

// permanentError : inutile de réessayer (URL inconnue, requête refusée).
type permanentError struct{ error }

// send fait une requête POST avec nouvelles tentatives sur les erreurs passagères.
func send(ctx context.Context, o Options, url string, body []byte, header http.Header) error {
	var last error
	for attempt := 0; attempt <= len(o.Backoff); attempt++ {
		if attempt > 0 {
			if err := o.Sleep(ctx, o.Backoff[attempt-1]); err != nil {
				return errors.Join(last, err)
			}
		}
		last = post(ctx, o, url, body, header)
		var perm permanentError
		if last == nil || errors.As(last, &perm) {
			return last
		}
	}
	return last
}

func post(ctx context.Context, o Options, url string, body []byte, header http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return permanentError{err}
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "kura-agent/"+o.Version)
	resp, err := o.Client.Do(req)
	if err != nil {
		return fmt.Errorf("envoi impossible : %w", redact(err, url))
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("réponse %d : %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	default:
		return permanentError{fmt.Errorf("réponse %d : %s", resp.StatusCode, strings.TrimSpace(string(msg)))}
	}
}

// redact retire l'URL (qui peut contenir un identifiant secret) des messages d'erreur.
func redact(err error, url string) error {
	return errors.New(strings.ReplaceAll(err.Error(), url, "<url>"))
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variante RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Succeeded : l'exécution compte comme sauvegarde réussie (tous les jobs OK ou avertissement).
func Succeeded(st state.Status) bool {
	return st.EndedAt != nil && st.LastSuccess != nil && st.LastSuccess.Equal(*st.EndedAt)
}

// Summary résume un bilan en texte court (journal healthchecks, journaux).
func Summary(st state.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kura-agent %s : %s (code %d)\n", st.RunID, st.Result, st.ExitCode)
	if st.Message != "" {
		fmt.Fprintf(&b, "%s\n", st.Message)
	}
	for _, p := range st.PreTasks {
		fmt.Fprintf(&b, "pré-tâche %s : %s, %d trouvé(s), %d arrêté(s)", p.Process, p.Result, p.Found, p.Closed)
		if p.Message != "" {
			fmt.Fprintf(&b, " — %s", p.Message)
		}
		b.WriteString("\n")
	}
	for _, j := range st.Jobs {
		fmt.Fprintf(&b, "job %s : %s, %d envoi(s), %d suppression(s), %.0f s", j.ID, j.Result, j.Uploaded, j.Deleted, j.DurationS)
		if j.Message != "" {
			fmt.Fprintf(&b, " — %s", j.Message)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Check vérifie une destination pour `kura-agent validate`, sans fausser la supervision :
// healthchecks reçoit un signal /log, qui s'inscrit au journal du check sans changer son
// état ; un webhook n'est pas appelé (le broker prendrait l'essai pour une vraie exécution).
func Check(ctx context.Context, r *config.Report, store creds.Store, o Options) (string, error) {
	rep, err := New(r, store, o)
	if err != nil {
		return "", err
	}
	switch h := rep.(type) {
	case *healthchecks:
		if err := send(ctx, h.opt, h.endpoint("/log"), []byte("kura-agent validate : test de liaison"),
			http.Header{"Content-Type": {"text/plain; charset=utf-8"}}); err != nil {
			return "", err
		}
		return "signal de test visible dans le journal du check (son état ne change pas)", nil
	default:
		return "secrets résolus ; webhook non appelé pour ne pas créer de faux bilan", nil
	}
}
