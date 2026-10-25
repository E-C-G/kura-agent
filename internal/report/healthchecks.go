package report

import (
	"context"
	"net/http"
	"net/url"

	"github.com/E-C-G/kura-agent/internal/state"
)

// healthchecks parle l'API de ping de healthchecks.io :
//
//	<url>/start?rid=…  début de l'exécution (mesure de durée, détection d'un blocage)
//	<url>?rid=…        succès
//	<url>/fail?rid=…   échec ; le corps (10 ko au plus) apparaît dans le journal du check
//
// Le même rid relie le début et la fin d'une exécution.
type healthchecks struct {
	url string
	rid string
	opt Options
}

// bodyLimit : healthchecks conserve les 10 premiers ko du corps.
const bodyLimit = 10_000

func (h *healthchecks) Name() string { return "healthchecks" }

func (h *healthchecks) endpoint(suffix string) string {
	return h.url + suffix + "?rid=" + url.QueryEscape(h.rid)
}

// Start : comme Finish, réservé aux exécutions complètes ; sinon healthchecks verrait
// une exécution commencée et jamais terminée, et alerterait à tort.
func (h *healthchecks) Start(ctx context.Context, _ state.Status, full bool) error {
	if !full {
		return nil
	}
	return send(ctx, h.opt, h.endpoint("/start"), nil, nil)
}

// Finish : seule une exécution complète renseigne le check. Une exécution partielle
// (--job) ne doit ni le passer au vert alors qu'un job n'a pas tourné, ni le passer
// au rouge pour une relance manuelle.
func (h *healthchecks) Finish(ctx context.Context, st state.Status, full bool) error {
	if !full {
		return nil
	}
	suffix := "/fail"
	if Succeeded(st) {
		suffix = ""
	}
	body := Summary(st)
	if len(body) > bodyLimit {
		body = body[:bodyLimit]
	}
	return send(ctx, h.opt, h.endpoint(suffix), []byte(body),
		http.Header{"Content-Type": {"text/plain; charset=utf-8"}})
}
