package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/report"
	"github.com/E-C-G/kura-agent/internal/restore"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// destCheck : destination vérifiée, prête à recevoir.
type destCheck struct {
	dir     string // chemin absolu
	exists  bool
	marker  restore.Marker
	resumed map[string]string // reprise : chemin → version déjà restaurée ; nil sinon
}

// marge d'espace libre demandée au-delà du volume à restaurer.
const spaceMargin = 1.10

// checkDest vérifie --to : hors des sources des jobs, pas un lien, absent, vide, ou
// laissé par la même restauration interrompue (reprise).
func checkDest(stderr io.Writer, d restoreDeps, cfg *config.Config, to string, targets []restore.Target, at time.Time) (*destCheck, int) {
	refuse := func(format string, a ...any) (*destCheck, int) {
		fmt.Fprintf(stderr, "Refusé : "+format+"\n", a...)
		return nil, exitRestoreRefused
	}
	dir := filepath.Clean(absLocal(to, d.windows))
	if t, err := restore.Resolve(cfg, dir, d.windows); err == nil {
		return refuse("%s est dans la source du job %s : la copie partirait à la sauvegarde suivante. Choisir un dossier hors des sources sauvegardées.", dispPath(dir, d.windows), t.Job.ID)
	}
	m := restore.Marker{Started: d.now()}
	for _, t := range targets {
		m.Targets = append(m.Targets, t.Local)
	}
	if !at.IsZero() {
		a := at
		m.At = &a
	}
	dc := &destCheck{dir: dir, marker: m}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dc, 0
	case err != nil:
		return refuse("%v", err)
	case fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0:
		return refuse("%s est un lien symbolique ou une jonction", dispPath(dir, d.windows))
	case !fi.IsDir():
		return refuse("%s est un fichier", dispPath(dir, d.windows))
	}
	dc.exists = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		return refuse("%v", err)
	}
	if len(entries) == 0 {
		return dc, 0
	}
	prev, done, err := restore.ReadMarker(dir)
	if err != nil {
		return refuse("%v", err)
	}
	if prev == nil {
		return refuse("%s n'est pas vide : choisir un dossier absent ou vide (rien n'est jamais écrasé)", dispPath(dir, d.windows))
	}
	if !prev.Same(m) {
		return refuse("%s contient une autre restauration inachevée (commencée le %s) : la relancer à l'identique pour la terminer, ou choisir un autre dossier",
			dispPath(dir, d.windows), prev.Started.Local().Format("02/01/2006 15:04"))
	}
	dc.marker.Started = prev.Started
	dc.resumed = done
	return dc, 0
}

// outName : dossier sous --to qui reçoit une cible (nom du dossier d'origine, ou du
// job avec --all).
func outName(t restore.Target, all bool) string {
	if all {
		return t.Job.ID
	}
	base := path.Base(t.Local)
	if restore.CheckRel(base) != "" {
		return t.Job.ID // racine d'un lecteur (D:), nom inutilisable
	}
	return base
}

// nearestExisting : le dossier lui-même ou son plus proche parent existant.
func nearestExisting(dir string) string {
	for p := dir; ; {
		if _, err := os.Lstat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// mkdirOwned crée dir et ses parents manquants avec le propriétaire du dossier existant.
func mkdirOwned(dir string, o restore.Owner) error {
	var missing []string
	for p := dir; ; {
		if _, err := os.Lstat(p); err == nil {
			break
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if o.Set {
			if err := os.Chmod(p, 0o755); err != nil {
				return err
			}
			if err := os.Lchown(p, o.UID, o.GID); err != nil {
				return err
			}
		}
	}
	return nil
}

func currentAccount() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "?"
}

func restoreTo(ctx context.Context, stdout, stderr io.Writer, d restoreDeps, cfg *config.Config, b restoreBucket,
	dc *destCheck, reports []listReport, at time.Time, all, yes, asJSON bool, args []string) int {
	disp := func(p string) string { return dispPath(p, d.windows) }
	refuse := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "Refusé : "+format+"\n", a...)
		return exitRestoreRefused
	}

	// Plan : place de chaque fichier sous la destination.
	var items []restore.Item
	for _, r := range reports {
		base := outName(r.target, all)
		single := !r.target.Root && len(r.files) == 1 && len(r.files[0].Key) == len(r.target.Key)
		for _, f := range r.files {
			out := base + "/" + f.Rel
			if single {
				out = f.Rel // la cible est un fichier : il arrive directement dans --to
			}
			items = append(items, restore.Item{File: f, Out: out})
		}
	}
	restore.MarkConflicts(items, d.foldCase)
	var count, problems int
	var total, need int64
	for _, it := range items {
		if it.Problem != "" {
			problems++
			continue
		}
		count++
		total += it.Version.Size
		if v, ok := dc.resumed[it.Out]; !ok || v != it.Version.VersionID {
			need += it.Version.Size
		}
	}
	when := stateLabel(at)
	if count == 0 {
		fmt.Fprintf(stdout, "Rien à restaurer : aucun fichier à la date demandée (%s).\n", when)
		for _, it := range items {
			fmt.Fprintf(stdout, "  %s : %s\n", disp(it.Out), it.Problem)
		}
		return exitRestoreOK
	}
	free, err := d.freeSpace(nearestExisting(dc.dir))
	if err != nil {
		return refuse("espace libre de %s inconnu : %v", disp(dc.dir), err)
	}
	if float64(need)*spaceMargin > float64(free) {
		return refuse("%s à restaurer, %s libres sur le volume de %s (marge de 10 %% comprise)", humanSize(need), humanSize(int64(free)), disp(dc.dir))
	}

	fmt.Fprintf(stdout, "Restauration de %d fichier(s), %s — état : %s\n", count, humanSize(total), when)
	for _, r := range reports {
		fmt.Fprintf(stdout, "  %s (job %s)\n", disp(r.target.Local), r.target.Job.ID)
	}
	fmt.Fprintf(stdout, "Vers %s (%s libres)\n", disp(dc.dir), humanSize(int64(free)))
	if dc.resumed != nil {
		fmt.Fprintf(stdout, "Reprise de la restauration commencée le %s : %d fichier(s) déjà restauré(s) seront sautés.\n",
			dc.marker.Started.Local().Format("02/01/2006 15:04"), len(dc.resumed))
	}
	if problems > 0 {
		fmt.Fprintf(stdout, "%d fichier(s) ne seront pas restaurés (noms refusés, détail au journal).\n", problems)
	}
	if code := confirmRestore(stderr, d, yes); code != 0 {
		return code
	}

	// Écriture.
	owner, err := d.owner(nearestExisting(dc.dir))
	if err != nil {
		return refuse("propriétaire de %s : %v", disp(dc.dir), err)
	}
	if err := mkdirOwned(dc.dir, owner); err != nil {
		return refuse("création de %s : %v", disp(dc.dir), err)
	}
	dest, err := restore.OpenDest(dc.dir, owner)
	if err != nil {
		return refuse("%v", err)
	}
	defer dest.Close()
	prog, err := dest.StartProgress(dc.marker)
	if err != nil {
		return refuse("%s : %v", disp(dc.dir), err)
	}
	return execute(ctx, stdout, stderr, d, cfg, b, &restorePlan{
		mode: "to", where: disp(dc.dir), at: at, reports: reports,
		batches: []batch{{dest: dest, items: items, resumed: dc.resumed, prog: prog, show: disp}},
	}, asJSON, args)
}

// stateLabel : date de l'état, pour les messages.
func stateLabel(at time.Time) string {
	if at.IsZero() {
		return "dernière sauvegarde"
	}
	return at.Local().Format("02/01/2006 15:04")
}

// confirmRestore demande confirmation, sauf --yes ; 0 si l'on continue.
func confirmRestore(stderr io.Writer, d restoreDeps, yes bool) int {
	if yes {
		return 0
	}
	if !d.interactive {
		fmt.Fprintln(stderr, "Refusé : pas de confirmation possible sans terminal : ajouter --yes")
		return exitRestoreRefused
	}
	ok, err := d.confirm("Restaurer ? [o/N] ")
	if err != nil || !ok {
		fmt.Fprintln(stderr, "Refusé : restauration non confirmée")
		return exitRestoreRefused
	}
	return 0
}

// batch : fichiers à écrire sous une même destination.
type batch struct {
	dest    *restore.Dest
	items   []restore.Item
	resumed map[string]string
	prog    *restore.Progress // --to : reprise ; nil en place
	show    func(out string) string
}

// keptItem : fichier laissé tel quel en place (présent, ou identique à la version), ou
// en échec avant toute lecture (fichier actuel illisible, lien).
type keptItem struct {
	show, label string
	item        restore.Item
	err         error
}

// restorePlan : restauration confirmée, prête à écrire.
type restorePlan struct {
	mode      string // to | in_place | replace
	where     string // destination affichée
	at        time.Time
	reports   []listReport
	batches   []batch
	kept      []keptItem
	preFailed []keptItem
}

// execute écrit les fichiers, tient le journal, l'état et le bilan, et résume.
func execute(ctx context.Context, stdout, stderr io.Writer, d restoreDeps, cfg *config.Config, b restoreBucket,
	p *restorePlan, asJSON bool, args []string) int {
	disp := func(s string) string { return dispPath(s, d.windows) }
	started := d.now()
	stamp := started.Format("20060102-1504")
	logPath := ""
	var logw io.Writer = io.Discard
	if err := os.MkdirAll(d.logsDir, 0o755); err == nil {
		logPath = filepath.Join(d.logsDir, "restore-"+started.Format("20060102-150405")+".log")
		if f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err == nil {
			defer f.Close()
			logw = f
		} else {
			logPath = ""
		}
	}
	fmt.Fprintf(logw, "kura-agent %s restore — %s\n", d.version, started.Local().Format("02/01/2006 15:04:05"))
	fmt.Fprintf(logw, "Commande : kura-agent restore %s\n", quoteArgs(args))
	fmt.Fprintf(logw, "Compte : %s\nÉtat : %s\nDestination : %s\n", d.account(), stateLabel(p.at), p.where)
	for _, r := range p.reports {
		fmt.Fprintf(logw, "Chemin : %s (job %s, préfixe %s)\n", disp(r.target.Local), r.target.Job.ID, r.target.Key)
	}
	line := func(show string, it restore.Item) string {
		return fmt.Sprintf("%s\tversion %s\tsauvegardé le %s\t%d", show, it.Version.VersionID,
			it.Version.LastModified.Local().Format("02/01/2006 15:04"), it.Version.Size)
	}
	for _, k := range p.kept {
		fmt.Fprintf(logw, "%s\t%s\n", k.label, line(k.show, k.item))
	}
	var restored, replaced, skipped, ignored, failed int
	var bytes int64
	var failures []string
	for _, k := range p.preFailed {
		failed++
		fmt.Fprintf(logw, "ÉCHEC\t%s\t%v\n", line(k.show, k.item), k.err)
		if len(failures) < 20 {
			failures = append(failures, fmt.Sprintf("  %s : %v", k.show, k.err))
		}
	}
	total := len(p.kept) + len(p.preFailed)
	for _, bt := range p.batches {
		total += len(bt.items)
	}
	done := len(p.kept) + len(p.preFailed)
	last := time.Now()
	for _, bt := range p.batches {
		bt.dest.Stamp = stamp
		restore.Restore(ctx, b, bt.dest, bt.items, d.workers, bt.resumed, func(o restore.Outcome) {
			it := o.Item
			l := line(bt.show(it.Out), it)
			done++
			switch {
			case o.Skipped:
				skipped++
				fmt.Fprintf(logw, "DÉJÀ\t%s\n", l)
			case o.Refused:
				ignored++
				fmt.Fprintf(logw, "REFUSÉ\t%s\t%v\n", l, o.Err)
			case o.Err != nil:
				failed++
				fmt.Fprintf(logw, "ÉCHEC\t%s\t%v\n", l, o.Err)
				if len(failures) < 20 {
					failures = append(failures, fmt.Sprintf("  %s : %v", bt.show(it.Out), o.Err))
				}
			case o.Backup != "":
				replaced++
				bytes += it.Version.Size
				fmt.Fprintf(logw, "REMPLACÉ\t%s\tactuel gardé : %s\n", l, bt.show(o.Backup))
			default:
				restored++
				bytes += it.Version.Size
				if bt.prog != nil {
					bt.prog.Done(it)
				}
				fmt.Fprintf(logw, "OK\t%s\n", l)
			}
			if !asJSON && time.Since(last) >= 10*time.Second {
				last = time.Now()
				fmt.Fprintf(stdout, "  … %d/%d fichier(s), %s\n", done, total, humanSize(bytes))
			}
		})
	}
	interrupted := ctx.Err() != nil
	for _, bt := range p.batches {
		if bt.prog != nil {
			bt.prog.Finish(failed == 0 && !interrupted)
		}
	}

	ended := d.now()
	info := report.RestoreInfo{Account: d.account(), Mode: p.mode, To: p.where, Files: restored + replaced, Replaced: replaced,
		Bytes: bytes, Skipped: skipped + len(p.kept), Ignored: ignored, Failed: failed, Result: "ok", ExitCode: exitRestoreOK,
		Log: logPath, Started: started, Ended: ended}
	for _, r := range p.reports {
		info.Paths = append(info.Paths, disp(r.target.Local))
	}
	if !p.at.IsZero() {
		a := p.at
		info.At = &a
	}
	switch {
	case interrupted:
		info.Result, info.ExitCode = "interrupted", exitRestorePartial
		info.Message = "Restauration interrompue : relancer la même commande pour reprendre."
	case failed > 0:
		info.Result, info.ExitCode = "partial", exitRestorePartial
		info.Message = "Des fichiers sont en échec (détail au journal) : relancer la même commande réessaie seulement ceux-là."
	}
	fmt.Fprintf(logw, "Résumé : %d restauré(s) dont %d remplacé(s) (%d octets), %d laissé(s) ou déjà restauré(s), %d nom(s) refusé(s), %d en échec — %s\n",
		info.Files, replaced, bytes, info.Skipped, ignored, failed, info.Result)
	if err := state.WriteAtomic(d.statusFile, info); err != nil {
		fmt.Fprintf(stderr, "État de la restauration non enregistré : %v\n", err)
	}
	rctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	if err := d.report(rctx, cfg, info); err != nil {
		fmt.Fprintf(stderr, "Bilan de la restauration non envoyé : %v\n", err)
	}
	cancel()

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(info)
		return info.ExitCode
	}
	fmt.Fprintf(stdout, "%d fichier(s) restauré(s), %s", info.Files, humanSize(bytes))
	if replaced > 0 {
		fmt.Fprintf(stdout, " (dont %d remplacé(s), l'actuel gardé à côté en .avant-restauration-%s)", replaced, stamp)
	}
	if skipped > 0 {
		fmt.Fprintf(stdout, ", %d déjà restauré(s)", skipped)
	}
	if ignored > 0 {
		fmt.Fprintf(stdout, ", %d nom(s) refusé(s)", ignored)
	}
	if failed > 0 {
		fmt.Fprintf(stdout, ", %d en échec", failed)
	}
	if p.mode == "to" {
		fmt.Fprintf(stdout, " — dans %s\n", p.where)
	} else {
		fmt.Fprintln(stdout, " — en place")
	}
	for _, f := range failures {
		fmt.Fprintln(stdout, f)
	}
	if failed > len(failures) {
		fmt.Fprintf(stdout, "  … et %d autre(s)\n", failed-len(failures))
	}
	if info.Message != "" {
		fmt.Fprintln(stdout, info.Message)
	}
	if logPath != "" {
		fmt.Fprintf(stdout, "Journal : %s\n", logPath)
	}
	return info.ExitCode
}

// quoteArgs : ligne de commande lisible pour le journal.
func quoteArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"") {
			a = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}

// printRestoreStatus : dernière restauration, pour kura-agent status.
func printRestoreStatus(w io.Writer, file string) {
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var r report.RestoreInfo
	if json.Unmarshal(data, &r) != nil || r.Ended.IsZero() {
		return
	}
	res := map[string]string{"ok": "OK", "partial": "partielle", "interrupted": "interrompue"}[r.Result]
	if res == "" {
		res = r.Result
	}
	fmt.Fprintf(w, "\nDernière restauration : %s par %s, %s\n", r.Ended.Local().Format("02/01/2006 15:04"), r.Account, res)
	fmt.Fprintf(w, "  %s → %s\n", strings.Join(r.Paths, ", "), r.To)
	fmt.Fprintf(w, "  %d fichier(s), %s", r.Files, humanSize(r.Bytes))
	if r.Ignored > 0 {
		fmt.Fprintf(w, ", %d nom(s) refusé(s)", r.Ignored)
	}
	if r.Failed > 0 {
		fmt.Fprintf(w, ", %d en échec", r.Failed)
	}
	fmt.Fprintln(w)
	if r.Log != "" {
		fmt.Fprintf(w, "  journal : %s\n", r.Log)
	}
}
