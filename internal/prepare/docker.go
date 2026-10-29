package prepare

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/docker"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Préparation docker_volume (Linux) : chaque volume Docker nommé devient <volume>.tar.gz,
// écrit par l'agent (propriétaires numériques, modes, dates, liens gardés comme liens,
// jamais suivis). Avec pause (défaut), les conteneurs qui montent les volumes sont gelés
// le temps de la lecture, au plus max_pause, puis toujours dégelés.

// DockerAPI : ce que la préparation demande à Docker (docker.Client en production).
type DockerAPI interface {
	Negotiate(ctx context.Context) (string, error)
	Volume(ctx context.Context, name string) (docker.Volume, error)
	Using(ctx context.Context, volumes []string) ([]docker.Container, error)
	ByID(ctx context.Context, ids []string) ([]docker.Container, error)
	Pause(ctx context.Context, id string) error
	Unpause(ctx context.Context, id string) error
}

// PausedEntry : un conteneur gelé par l'agent, noté avant le gel pour être dégelé par
// l'exécution suivante si l'agent est tué entre-temps.
type PausedEntry struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Prepare string    `json:"prepare"`
	Socket  string    `json:"socket"`
	At      time.Time `json:"at"`
}

// Client : client de l'API pour le socket, après contrôle du socket.
func (d Deps) Client(socket string) (DockerAPI, error) { return d.dockerClient(socket) }

func (d Deps) dockerClient(socket string) (DockerAPI, error) {
	check := d.CheckSocket
	if check == nil {
		check = docker.CheckSocket
	}
	if err := check(socket); err != nil {
		return nil, err
	}
	if d.Docker != nil {
		return d.Docker(socket), nil
	}
	return docker.New(socket), nil
}

// volumeArchive : nom de l'archive d'un volume.
func volumeArchive(name string) string { return name + ".tar.gz" }

// runDocker archive les volumes de p dans tmp ; seal, s'il est donné, chiffre chaque
// archive au fil de l'eau (préparation age dont l'entrée est p).
func runDocker(ctx context.Context, cfg *config.Config, p config.Prepare, d Deps, tmp string, ps *state.PrepareStatus, log *slog.Logger,
	seal func(io.Writer) (io.WriteCloser, error)) error {
	if d.LastSize > 0 {
		need := MinFree + uint64(d.LastSize)/5*6
		if free, err := d.FreeSpace(d.StagingDir); err == nil && free < need {
			return &lowSpaceError{free: free, need: need, hint: fmt.Sprintf("dernières archives %s, plus 20 %%, plus %s de réserve", humanSize(uint64(d.LastSize)), humanSize(MinFree))}
		}
	}
	type vol struct{ name, dir string }
	var vols []vol
	var api DockerAPI
	if p.Paused() {
		var err error
		if api, err = d.dockerClient(p.Socket); err != nil {
			return err
		}
		version, err := api.Negotiate(ctx)
		if err != nil {
			return err
		}
		log.Info("Docker", "version", version, "socket", p.Socket)
		for _, name := range p.Volumes {
			v, err := api.Volume(ctx, name)
			if errors.Is(err, docker.ErrNotFound) {
				return fmt.Errorf("volume Docker %q introuvable", name)
			}
			if err != nil {
				return fmt.Errorf("volume %s : %w", name, err)
			}
			if v.Driver != "local" {
				return fmt.Errorf("volume %s : pilote %q, seul le pilote local est pris en charge", name, v.Driver)
			}
			if !mountpointOK(v.Mountpoint, name) {
				return fmt.Errorf("volume %s : emplacement %q inattendu (…/volumes/%s/_data attendu)", name, v.Mountpoint, name)
			}
			vols = append(vols, vol{name, v.Mountpoint})
		}
	} else {
		for _, name := range p.Volumes {
			vols = append(vols, vol{name, filepath.Join(p.DataRoot, "volumes", name, "_data")})
		}
	}
	for _, v := range vols {
		fi, err := os.Lstat(v.dir)
		switch {
		case errors.Is(err, os.ErrNotExist) && !p.Paused():
			return fmt.Errorf("volume %s : %s absent (volume inconnu, ou data_root à corriger)", v.name, v.dir)
		case err != nil:
			return fmt.Errorf("volume %s : %w", v.name, err)
		case !fi.IsDir():
			return fmt.Errorf("volume %s : %s n'est pas un dossier (lien refusé)", v.name, v.dir)
		}
	}

	// Gel.
	var paused []docker.Container
	if api != nil {
		var err error
		if paused, err = freeze(ctx, api, p, d, log); err != nil {
			return err
		}
		start := d.Now()
		for _, k := range paused {
			ps.Paused = append(ps.Paused, k.Name())
		}
		defer func() {
			ps.PauseS = d.Now().Sub(start).Seconds()
		}()
	}
	thawed := false
	thaw := func() error {
		if thawed || api == nil {
			return nil
		}
		thawed = true
		return unfreeze(api, paused, d, log)
	}
	defer thaw()

	// Lecture, pendant le gel, au plus max_pause.
	rctx, cancel := ctx, context.CancelFunc(func() {})
	if api != nil && p.MaxPause.Duration > 0 {
		rctx, cancel = context.WithTimeoutCause(ctx, p.MaxPause.Duration, fmt.Errorf("gel de plus de %s (max_pause) : lecture arrêtée", p.MaxPause.Duration))
	}
	defer cancel()
	excluded := excluder(p.Exclude)
	var notes []string
	results := make([]tarStats, len(vols))
	for i, v := range vols {
		name := volumeArchive(v.name)
		if seal != nil {
			name += sealExt
		}
		st, err := writeVolume(rctx, v.dir, excluded, filepath.Join(tmp, name), seal)
		if err != nil {
			if cause := context.Cause(rctx); rctx.Err() != nil && ctx.Err() == nil && cause != nil {
				err = cause
			}
			return fmt.Errorf("volume %s : %w", v.name, err)
		}
		results[i] = st
		log.Info("volume archivé", "volume", v.name, "entrées", st.entries, "octets lus", st.bytes, "liens", st.links)
	}
	if err := thaw(); err != nil {
		return err
	}

	// Contrôle, après le dégel.
	for i, v := range vols {
		name := volumeArchive(v.name)
		path := filepath.Join(tmp, name)
		if seal != nil {
			path += sealExt
		} else if n, err := verifyTarGz(path); err != nil {
			return fmt.Errorf("archive %s illisible après écriture : %w", name, err)
		} else if n != results[i].entries {
			return fmt.Errorf("archive %s : %d entrées relues, %d écrites", name, n, results[i].entries)
		}
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		st := results[i]
		ps.Volumes = append(ps.Volumes, state.VolumeStatus{Name: v.name, Size: fi.Size(), Entries: st.entries})
		ps.Files++
		ps.Size += fi.Size()
		if st.skipped > 0 {
			notes = append(notes, fmt.Sprintf("%s : %d tube(s), socket(s) ou périphérique(s) laissé(s) de côté", v.name, st.skipped))
		}
		if len(st.changed) > 0 {
			notes = append(notes, fmt.Sprintf("%s : %d fichier(s) changé(s) pendant la lecture (%s)", v.name, len(st.changed), sample(st.changed)))
		}
		if st.entries <= 1 {
			notes = append(notes, v.name+" : volume vide")
		}
	}
	if !p.Paused() {
		notes = append([]string{"copie non figée (pause: false) : un conteneur a pu écrire pendant la lecture"}, notes...)
	}
	if len(notes) > 0 {
		ps.Result, ps.Message = state.ResultWarning, strings.Join(notes, " ; ")
	}
	return nil
}

// mountpointOK : emplacement d'un volume du pilote local, …/volumes/<nom>/_data.
func mountpointOK(mp, name string) bool {
	return strings.HasPrefix(mp, "/") && filepath.Clean(mp) == mp &&
		strings.HasSuffix(mp, "/volumes/"+name+"/_data") && len(mp) > len("/volumes/"+name+"/_data")
}

// freeze gèle les conteneurs en marche qui montent les volumes de p, après les avoir
// notés dans le fichier de reprise ; vérifie que le gel a pris.
func freeze(ctx context.Context, api DockerAPI, p config.Prepare, d Deps, log *slog.Logger) ([]docker.Container, error) {
	all, err := api.Using(ctx, p.Volumes)
	if err != nil {
		return nil, fmt.Errorf("conteneurs des volumes : %w", err)
	}
	var run []docker.Container
	for _, k := range all {
		switch k.State {
		case "running":
			run = append(run, k)
		case "paused":
			log.Info("conteneur déjà en pause, laissé tel quel", "conteneur", k.Name())
		}
	}
	if len(run) == 0 {
		log.Info("aucun conteneur en marche sur ces volumes : rien à geler")
		return nil, nil
	}
	if err := notePaused(d, p, run); err != nil {
		return nil, fmt.Errorf("fichier de reprise du gel : %w", err)
	}
	var done []docker.Container
	fail := func(err error) ([]docker.Container, error) {
		unfreeze(api, done, d, log)
		return nil, err
	}
	for _, k := range run {
		if err := api.Pause(ctx, k.ID); err != nil {
			return fail(fmt.Errorf("gel de %s : %w", k.Name(), err))
		}
		done = append(done, k)
	}
	ids := make([]string, len(done))
	for i, k := range done {
		ids[i] = k.ID
	}
	now, err := api.ByID(ctx, ids)
	if err != nil {
		return fail(fmt.Errorf("état des conteneurs après le gel : %w", err))
	}
	state := map[string]string{}
	for _, k := range now {
		state[k.ID] = k.State
	}
	for _, k := range done {
		if state[k.ID] != "paused" {
			return fail(fmt.Errorf("%s n'est pas gelé (état %q) : lecture non faite", k.Name(), state[k.ID]))
		}
	}
	var names []string
	for _, k := range done {
		names = append(names, k.Name())
	}
	log.Info("conteneurs gelés", "conteneurs", names)
	return done, nil
}

// unfreeze dégèle les conteneurs, même si le contexte de l'exécution est annulé, puis
// efface le fichier de reprise quand tout est dégelé.
func unfreeze(api DockerAPI, ks []docker.Container, d Deps, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var errs []error
	for _, k := range ks {
		if err := api.Unpause(ctx, k.ID); err != nil {
			errs = append(errs, fmt.Errorf("dégel de %s : %w", k.Name(), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		log.Error("dégel incomplet", "err", err)
		return fmt.Errorf("%w ; le dégeler à la main (docker unpause), l'exécution suivante réessaiera", err)
	}
	if len(ks) > 0 {
		log.Info("conteneurs dégelés", "nombre", len(ks))
	}
	if d.PausedFile != "" {
		if err := os.Remove(d.PausedFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func notePaused(d Deps, p config.Prepare, ks []docker.Container) error {
	if d.PausedFile == "" {
		return nil
	}
	var list []PausedEntry
	for _, k := range ks {
		list = append(list, PausedEntry{ID: k.ID, Name: k.Name(), Prepare: p.ID, Socket: p.Socket, At: d.Now()})
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.PausedFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.PausedFile)
}

// RecoverPaused dégèle les conteneurs qu'une exécution interrompue a laissés gelés
// (fichier de reprise), s'ils sont encore en pause ; renvoie leurs noms. Le fichier est
// effacé une fois tout dégelé.
func RecoverPaused(ctx context.Context, file string, client func(socket string) (DockerAPI, error)) ([]string, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []PausedEntry
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s illisible : %w", file, err)
	}
	bySocket := map[string][]PausedEntry{}
	for _, e := range list {
		bySocket[e.Socket] = append(bySocket[e.Socket], e)
	}
	var thawed []string
	var errs []error
	for socket, es := range bySocket {
		api, err := client(socket)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := api.Negotiate(ctx); err != nil {
			errs = append(errs, err)
			continue
		}
		ids := make([]string, len(es))
		for i, e := range es {
			ids[i] = e.ID
		}
		now, err := api.ByID(ctx, ids)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, k := range now {
			if k.State != "paused" {
				continue // dégelé entre-temps, ou arrêté : rien à faire
			}
			if err := api.Unpause(ctx, k.ID); err != nil {
				errs = append(errs, fmt.Errorf("dégel de %s : %w", k.Name(), err))
				continue
			}
			thawed = append(thawed, k.Name())
		}
	}
	if err := errors.Join(errs...); err != nil {
		return thawed, err
	}
	return thawed, os.Remove(file)
}

// excluder : motifs d'exclusion appliqués aux chemins relatifs d'un volume.
func excluder(patterns []string) func(rel string, dir bool) bool {
	var res []*regexp.Regexp
	for _, p := range patterns {
		res = append(res, regexp.MustCompile("^"+globRegexp(p)+"$"))
	}
	return func(rel string, dir bool) bool {
		for _, re := range res {
			if re.MatchString(rel) || dir && re.MatchString(rel+"/") {
				return true
			}
		}
		return false
	}
}

// globRegexp : motif de la CLI AWS (* : n'importe quoi, ? : un caractère, sensible à la
// casse sous Linux).
func globRegexp(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}

// tarStats : ce qu'une archive de volume contient.
type tarStats struct {
	entries, links, skipped int
	bytes                   int64
	changed                 []string
}

// writeVolume écrit l'archive du dossier dir dans path (tar, gzip rapide, puis seal).
func writeVolume(ctx context.Context, dir string, excluded func(string, bool) bool, path string,
	seal func(io.Writer) (io.WriteCloser, error)) (tarStats, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return tarStats{}, err
	}
	var out io.WriteCloser = f
	closers := []io.Closer{}
	if seal != nil {
		w, err := seal(f)
		if err != nil {
			f.Close()
			return tarStats{}, err
		}
		out = w
		closers = append(closers, w)
	}
	gz, _ := gzip.NewWriterLevel(out, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	st, err := tarTree(ctx, dir, excluded, tw)
	errs := []error{err, tw.Close(), gz.Close()}
	for _, c := range closers {
		errs = append(errs, c.Close())
	}
	errs = append(errs, f.Close())
	if err := errors.Join(errs...); err != nil {
		os.Remove(path)
		return st, err
	}
	return st, nil
}

// verifyTarGz relit toute l'archive (sommes gzip, en-têtes tar, données) et renvoie le
// nombre d'entrées.
func verifyTarGz(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return n, err
		}
		n++
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return n, err
	}
	return n, gz.Close()
}
