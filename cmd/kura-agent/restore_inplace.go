package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/restore"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// inPlaceJob : fichiers d'un job à remettre sous sa source.
type inPlaceJob struct {
	report listReport
	source string // dossier source du job, tel que le système l'écrit
	dest   *restore.Dest
	items  []restore.Item // à écrire ou remplacer, et noms refusés
	need   int64
}

// restoreInPlace remet les fichiers à leur place, sous la source de leur job. Sans
// --replace, seuls les fichiers absents sont remis ; avec, ceux qui diffèrent sont aussi
// remplacés, l'actuel gardé à côté. Le verrou de la sauvegarde est tenu par l'appelant.
func restoreInPlace(ctx context.Context, stdout, stderr io.Writer, d restoreDeps, cfg *config.Config, b restoreBucket,
	reports []listReport, at time.Time, replace, list, yes, asJSON bool, args []string) int {
	disp := func(p string) string { return dispPath(p, d.windows) }
	refuse := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "Refusé : "+format+"\n", a...)
		return exitRestoreRefused
	}
	owner := d.inPlaceOwner()
	var jobs []*inPlaceJob
	var kept, preFailed []keptItem
	var nWrite, nReplace, nKeep, nSame, nRefused int
	var sWrite, sReplace int64
	defer func() {
		for _, j := range jobs {
			if j.dest != nil {
				j.dest.Close()
			}
		}
	}()
	for _, r := range reports {
		src := filepath.Clean(r.target.Job.Source)
		dest, err := restore.OpenDest(src, owner)
		if err != nil {
			return refuse("source du job %s inutilisable (%v) : restaurer dans un dossier à part avec --to", r.target.Job.ID, err)
		}
		j := &inPlaceJob{report: r, source: src, dest: dest}
		jobs = append(jobs, j)
		root := strings.Trim(r.target.Job.Destination, "/")
		var items []restore.Item
		for _, f := range r.files {
			out := f.Key
			if root != "" {
				out = strings.TrimPrefix(f.Key, root+"/")
			}
			items = append(items, restore.Item{File: f, Out: out})
		}
		restore.MarkConflicts(items, d.foldCase)
		show := func(out string) string { return disp(underSource(src, out)) }
		for _, it := range items {
			if it.Problem != "" {
				nRefused++
				j.items = append(j.items, it)
				continue
			}
			act, err := dest.Classify(it, replace)
			switch {
			case err != nil:
				preFailed = append(preFailed, keptItem{show: show(it.Out), label: "ÉCHEC", item: it, err: err})
			case act == restore.ActWrite:
				nWrite++
				sWrite += it.Version.Size
				j.need += it.Version.Size
				j.items = append(j.items, it)
			case act == restore.ActReplace:
				nReplace++
				sReplace += it.Version.Size
				j.need += it.Version.Size
				it.Replace = true
				j.items = append(j.items, it)
			case act == restore.ActKeep:
				nKeep++
				kept = append(kept, keptItem{show: show(it.Out), label: "PRÉSENT", item: it})
			case act == restore.ActSame:
				nSame++
				kept = append(kept, keptItem{show: show(it.Out), label: "IDENTIQUE", item: it})
			}
		}
	}

	when := stateLabel(at)
	fmt.Fprintf(stdout, "Restauration en place — état : %s\n", when)
	for _, j := range jobs {
		fmt.Fprintf(stdout, "  %s (job %s)\n", disp(j.report.target.Local), j.report.target.Job.ID)
	}
	fmt.Fprintf(stdout, "  à remettre (absents)       : %d fichier(s), %s\n", nWrite, humanSize(sWrite))
	if replace {
		fmt.Fprintf(stdout, "  à remplacer (différents)   : %d fichier(s), %s — l'actuel gardé à côté (.avant-restauration-<date>)\n", nReplace, humanSize(sReplace))
		fmt.Fprintf(stdout, "  laissés (identiques)       : %d\n", nSame)
	} else {
		fmt.Fprintf(stdout, "  laissés (présents)         : %d — --replace pour remplacer ceux qui diffèrent\n", nKeep)
	}
	if nRefused > 0 {
		fmt.Fprintf(stdout, "  noms refusés               : %d\n", nRefused)
	}
	if len(preFailed) > 0 {
		fmt.Fprintf(stdout, "  illisibles ou bloqués      : %d\n", len(preFailed))
		for i, f := range preFailed {
			if i == 10 {
				fmt.Fprintf(stdout, "    … et %d autre(s)\n", len(preFailed)-10)
				break
			}
			fmt.Fprintf(stdout, "    %s : %v\n", f.show, f.err)
		}
	}
	if list {
		for _, j := range jobs {
			for _, it := range j.items {
				what := "remis"
				switch {
				case it.Problem != "":
					what = "refusé : " + it.Problem
				case it.Replace:
					what = "remplacé"
				}
				fmt.Fprintf(stdout, "  %-60s %10s  %s\n", disp(underSource(j.source, it.Out)), humanSize(it.Version.Size), what)
			}
		}
		return exitRestoreOK
	}
	if nWrite+nReplace == 0 {
		fmt.Fprintln(stdout, "Rien à remettre en place.")
		if len(preFailed) > 0 {
			return exitRestorePartial
		}
		return exitRestoreOK
	}
	for _, j := range jobs {
		if j.need == 0 {
			continue
		}
		free, err := d.freeSpace(j.source)
		if err != nil {
			return refuse("espace libre de %s inconnu : %v", disp(j.source), err)
		}
		if float64(j.need)*spaceMargin > float64(free) {
			return refuse("%s à restaurer, %s libres sur le volume de %s (marge de 10 %% comprise)", humanSize(j.need), humanSize(int64(free)), disp(j.source))
		}
	}
	if code := confirmRestore(stderr, d, yes); code != 0 {
		return code
	}
	mode := "in_place"
	if replace {
		mode = "replace"
	}
	p := &restorePlan{mode: mode, where: "en place", at: at, reports: reports, kept: kept, preFailed: preFailed}
	for _, j := range jobs {
		src := j.source
		p.batches = append(p.batches, batch{dest: j.dest, items: j.items,
			show: func(out string) string { return disp(underSource(src, out)) }})
	}
	return execute(ctx, stdout, stderr, d, cfg, b, p, asJSON, args)
}

// underSource : chemin d'un fichier sous la source, sans le nettoyer (un nom refusé
// comme ../x reste lisible tel qu'il est dans le bucket).
func underSource(src, out string) string {
	return strings.TrimRight(src, `/\`) + string(filepath.Separator) + filepath.FromSlash(out)
}
