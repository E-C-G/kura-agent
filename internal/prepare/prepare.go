// Package prepare fait les préparations de kura-agent : avant les jobs, il écrit dans la
// zone de préparation de l'agent ce que la synchronisation seule ne sait pas sauvegarder
// proprement (archive zip d'un dossier ; plus tard exports de bases et chiffrés). Un job
// ordinaire envoie ensuite ce dossier au bucket (source ${STAGING:<id>}).
//
// Chaque préparation écrit dans <zone>/<id>.tmp, puis, réussie, le renomme en <zone>/<id> :
// un job ne voit jamais un résultat partiel. La zone est réservée à l'agent et vidée à la
// fin de chaque exécution.
package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/diskfree"
	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// MinFree : espace que les préparations laissent toujours libre sur le disque de la zone
// de préparation, qui est sous Windows le disque système : plein, il bloquerait le poste
// entier, pas seulement la sauvegarde.
const MinFree uint64 = 2 << 30

// lowSpaceError : l'espace libre de la zone de préparation est passé sous MinFree.
type lowSpaceError struct{ free uint64 }

func (e *lowSpaceError) Error() string {
	return fmt.Sprintf("espace disque insuffisant dans la zone de préparation : %s libres, %s à garder sur ce disque ; prévoir deux fois la taille de la source",
		humanSize(e.free), humanSize(MinFree))
}

// humanSize : taille lisible, à la française (1,2 Go).
func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d o", n)
	}
	v, i := float64(n)/unit, 0
	for v >= unit && i < 3 {
		v /= unit
		i++
	}
	return strings.Replace(fmt.Sprintf("%.1f %s", v, []string{"Ko", "Mo", "Go", "To"}[i]), ".", ",", 1)
}

// Runner lance un outil (execx.System en production).
type Runner interface {
	RunSpec(ctx context.Context, s execx.Spec) (int, error)
}

// Deps sont les dépendances d'une préparation.
type Deps struct {
	Exec       Runner
	StagingDir string
	// AgentDirs : dossiers de l'agent (données, coffre, configuration, journaux),
	// jamais archivés même s'ils se trouvent sous une source.
	AgentDirs []string
	// CheckTool vérifie l'outil avant usage (défaut : CheckZip) ; remplaçable en test,
	// où les outils ne sont pas à root.
	CheckTool func(ctx context.Context, r Runner, path string) (string, error)
	Out       io.Writer // sortie des outils (journal de la préparation)
	Log       *slog.Logger
	Now       func() time.Time

	// FreeSpace lit l'espace libre du disque de la zone de préparation (défaut :
	// diskfree.Free), surveillé toutes les SpaceCheck (défaut 2 s) pendant la
	// préparation : sous MinFree, elle s'arrête et échoue.
	FreeSpace  func(dir string) (uint64, error)
	SpaceCheck time.Duration

	// Copied, s'il est donné, est appelé une fois la copie de la source terminée,
	// réussie ou non : un programme fermé pour elle (pre_tasks) peut alors être relancé
	// sans attendre l'archive ni l'envoi.
	Copied func()
}

// Run fait une préparation et renvoie son résultat ; le dossier <zone>/<id> n'existe
// qu'en cas de réussite (ou d'avertissement).
func Run(ctx context.Context, cfg *config.Config, p config.Prepare, d Deps) (ps state.PrepareStatus) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.Out == nil {
		d.Out = io.Discard
	}
	if d.FreeSpace == nil {
		d.FreeSpace = diskfree.Free
	}
	if d.SpaceCheck <= 0 {
		d.SpaceCheck = 2 * time.Second
	}
	ps = state.PrepareStatus{ID: p.ID, Type: p.Type, Result: state.ResultOK}
	start := d.Now()
	defer func() { ps.DurationS = d.Now().Sub(start).Seconds() }()
	log := d.Log.With("préparation", p.ID)

	fail := func(format string, a ...any) state.PrepareStatus {
		ps.Result, ps.Message = state.ResultError, fmt.Sprintf(format, a...)
		var low *lowSpaceError
		switch {
		case errors.As(context.Cause(ctx), &low):
			// Copie ou outil arrêtés faute de place : l'erreur qu'ils rendent (« context
			// canceled ») n'apprend rien, la cause seule est utile.
			ps.Message = low.Error()
		case ctx.Err() != nil:
			ps.Message += " (" + interruption(ctx) + ")"
		}
		log.Error("préparation en échec", "raison", ps.Message)
		return ps
	}
	if p.Unavailable != "" {
		return fail("%s", p.Unavailable)
	}
	if d.StagingDir == "" {
		return fail("zone de préparation inconnue")
	}
	if p.Timeout.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout.Duration)
		defer cancel()
	}
	tmp, final := filepath.Join(d.StagingDir, p.ID+".tmp"), filepath.Join(d.StagingDir, p.ID)
	for _, dir := range []string{tmp, final} {
		if err := os.RemoveAll(dir); err != nil {
			return fail("nettoyage de %s : %v", dir, err)
		}
	}
	switch free, err := d.FreeSpace(d.StagingDir); {
	case err != nil:
		log.Warn("espace libre de la zone de préparation illisible : non surveillé", "err", err)
	case free < MinFree:
		return fail("%v", &lowSpaceError{free: free})
	default:
		var stop func()
		ctx, stop = watchSpace(ctx, d)
		defer stop()
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fail("dossier de travail : %v", err)
	}
	defer os.RemoveAll(tmp) // sans effet après le renommage

	var err error
	switch p.Type {
	case config.PrepareZip:
		err = runZip(ctx, cfg, p, d, tmp, &ps, log)
	default:
		err = fmt.Errorf("type %q non pris en charge par cette version de l'agent", p.Type)
	}
	if err != nil {
		return fail("%v", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fail("mise en place du résultat : %v", err)
	}
	log.Info("préparation terminée", "résultat", ps.Result, "fichiers", ps.Files, "taille", ps.Size,
		"manquants", ps.Missing)
	return ps
}

// watchSpace surveille l'espace libre de la zone de préparation : passé sous MinFree, le
// contexte rendu est annulé avec lowSpaceError pour cause (la copie s'arrête, l'outil est
// arrêté), et la préparation échoue ; ses fichiers sont effacés comme pour tout échec.
// stop arrête la surveillance.
func watchSpace(ctx context.Context, d Deps) (watched context.Context, stop func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	quit, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(d.SpaceCheck)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if free, err := d.FreeSpace(d.StagingDir); err == nil && free < MinFree {
					cancel(&lowSpaceError{free: free})
					return
				}
			}
		}
	}()
	return ctx, func() {
		close(quit)
		<-finished
		cancel(nil)
	}
}

// interruption décrit pourquoi ctx s'est arrêté.
func interruption(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "durée maximale dépassée"
	}
	return "interrompue"
}

// Clear vide la zone de préparation (fin d'exécution, ou reste d'une exécution
// interrompue) sans supprimer la zone elle-même.
func Clear(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
