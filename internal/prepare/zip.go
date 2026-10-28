package prepare

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Codes de sortie « un fichier n'a pas pu être lu » : acceptés avec on_locked: skip.
const (
	infoZipWarning = 18 // zip : fichier illisible
	sevenZipWarn   = 1  // 7z : avertissement (fichier verrouillé…)
)

// runZip archive p.Source dans <tmp>/<id>.zip avec tools.zip (Info-ZIP ou 7-Zip).
//
// L'agent copie d'abord les fichiers retenus dans la zone de préparation (copySource :
// liens et jonctions jamais suivis, exclusions, dossiers de l'agent laissés de côté,
// chaque fichier ouvert sous la source). L'outil archive cette copie, que nul autre que
// l'agent ne peut modifier, d'après la liste des fichiers copiés. Puis l'agent relit
// l'archive en entier (sommes CRC) et vérifie qu'elle ne contient rien d'autre.
func runZip(ctx context.Context, cfg *config.Config, p config.Prepare, d Deps, tmp string, ps *state.PrepareStatus, log *slog.Logger) error {
	tool := cfg.Tools.Zip
	check := d.CheckTool
	if check == nil {
		check = CheckZip
	}
	version, err := check(ctx, d.Exec, tool)
	if err != nil {
		return fmt.Errorf("tools.zip : %w", err)
	}
	log.Info("outil zip", "chemin", tool, "version", version)

	flavor := config.ZipFlavor(tool)
	copyDir := filepath.Join(d.StagingDir, p.ID+".src")
	if err := os.RemoveAll(copyDir); err != nil {
		return err
	}
	if err := os.Mkdir(copyDir, 0o700); err != nil {
		return fmt.Errorf("copie de la source : %w", err)
	}
	defer os.RemoveAll(copyDir)
	c, err := copySource(ctx, p.Source, p, d.AgentDirs, flavor, copyDir)
	if err != nil {
		return err
	}
	if len(c.links) > 0 {
		log.Warn("liens et jonctions non suivis, laissés hors de l'archive", "liens", c.links)
	}
	if len(c.agent) > 0 {
		log.Warn("dossiers de l'agent laissés hors de l'archive", "chemins", c.agent)
	}
	if len(c.ignored) > 0 {
		log.Info("ni fichiers ni dossiers (tubes, périphériques), laissés de côté", "chemins", c.ignored)
	}
	lost := append(append([]string(nil), c.missing...), c.refused...)
	if len(lost) > 0 && p.OnLocked == config.OnLockedFail {
		return fmt.Errorf("%d fichier(s) ou dossier(s) illisible(s) ou au nom impossible à archiver (%s)", len(lost), sample(lost))
	}
	names := c.names
	if len(names) == 0 {
		return fmt.Errorf("rien à archiver dans %s : source vide ou entièrement exclue", p.Source)
	}
	log.Info("source copiée dans la zone de préparation", "fichiers", len(names), "octets", c.bytes)

	archive := filepath.Join(tmp, p.ID+".zip")
	// L'outil travaille sur la copie, que seul l'agent peut modifier : dossier de travail
	// (premier lieu où Windows cherche une DLL après le sien) et fichiers lus.
	spec := execx.Spec{Name: tool, Env: toolEnv(tmp), Dir: copyDir, Out: d.Out}
	warnCode := infoZipWarning
	switch flavor {
	case config.ZipInfoZIP:
		level := "-6"
		if p.Level == config.LevelStore {
			level = "-0"
		}
		// -y : un lien est archivé comme lien, jamais suivi ; -nw : pas de motifs, chaque
		// nom est pris tel quel ; -X : sans attributs propres au système ; -@ : noms sur
		// l'entrée standard.
		spec.Args = []string{"-q", "-y", "-X", "-nw", level, archive, "-@"}
		spec.Stdin = strings.NewReader(strings.Join(names, "\n") + "\n")
	case config.Zip7Zip:
		list := filepath.Join(d.StagingDir, p.ID+".list")
		native := make([]string, len(names))
		for i, n := range names {
			native[i] = filepath.FromSlash(n)
		}
		if err := os.WriteFile(list, []byte(strings.Join(native, "\n")+"\n"), 0o600); err != nil {
			return fmt.Errorf("liste des fichiers : %w", err)
		}
		defer os.Remove(list)
		level := "-mx5"
		if p.Level == config.LevelStore {
			level = "-mx0"
		}
		// -snl : lien archivé comme lien ; -spd : pas de motifs dans les noms ; -ssw : lit
		// aussi les fichiers ouverts en écriture ; -mcu=on : noms en UTF-8.
		spec.Args = []string{"a", "-tzip", "-snl", "-spd", "-ssw", "-mcu=on", level, "-bd", "-y", "-scsUTF-8", archive, "@" + list}
		warnCode = sevenZipWarn
	default:
		return fmt.Errorf("tools.zip : %q n'est ni zip (Info-ZIP) ni 7z (7-Zip)", tool)
	}
	fmt.Fprintf(d.Out, "== archive %s : %d fichier(s) de %s ==\n", p.ID, len(names), p.Source)
	code, err := d.Exec.RunSpec(ctx, spec)
	if err != nil {
		return fmt.Errorf("%s : %w", filepath.Base(tool), err)
	}
	switch {
	case code == 0:
	case code == warnCode && p.OnLocked == config.OnLockedSkip:
		log.Warn("fichiers illisibles laissés de côté (on_locked: skip)", "code", code)
	default:
		return fmt.Errorf("%s a échoué (code %d) ; voir le journal de la préparation", filepath.Base(tool), code)
	}

	entries, size, err := verifyZip(archive)
	if err != nil {
		return fmt.Errorf("archive illisible après écriture : %w", err)
	}
	missing, extra := compare(names, entries)
	if len(extra) > 0 {
		return fmt.Errorf("l'archive contient %d entrée(s) non demandée(s) (%s) : refusée", len(extra), sample(extra))
	}
	if len(missing) > 0 && p.OnLocked == config.OnLockedFail {
		return fmt.Errorf("%d fichier(s) absent(s) de l'archive (%s)", len(missing), sample(missing))
	}
	ps.Files, ps.Size = len(entries), size
	if lost = append(lost, missing...); len(lost) > 0 {
		ps.Missing = len(lost)
		ps.Result = state.ResultWarning
		ps.Message = fmt.Sprintf("%d fichier(s) illisible(s) laissé(s) de côté (%s)", len(lost), sample(lost))
	}
	return nil
}

// verifyZip relit toute l'archive (sommes CRC contrôlées par archive/zip à la fin de
// chaque entrée) et renvoie ses fichiers (hors entrées de dossier) et sa taille.
func verifyZip(path string) ([]string, int64, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		name := f.Name
		if runtime.GOOS == "windows" { // un « \ » n'est un séparateur que sous Windows
			name = strings.ReplaceAll(name, `\`, "/")
		}
		name = strings.TrimPrefix(name, "./")
		if strings.HasSuffix(name, "/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, 0, fmt.Errorf("%s : %w", name, err)
		}
		_, err = io.Copy(io.Discard, rc)
		rc.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("%s : %w", name, err)
		}
		names = append(names, name)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	return names, st.Size(), nil
}

// compare : fichiers demandés absents de l'archive, et entrées que personne n'a demandées.
func compare(want, got []string) (missing, extra []string) {
	asked := make(map[string]bool, len(want))
	for _, w := range want {
		asked[w] = true
	}
	seen := make(map[string]bool, len(got))
	for _, g := range got {
		if !asked[g] || seen[g] {
			extra = append(extra, g)
		}
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			missing = append(missing, w)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// sample : les premiers noms d'une liste, pour un message lisible.
func sample(list []string) string {
	const max = 5
	if len(list) <= max {
		return strings.Join(list, ", ")
	}
	return strings.Join(list[:max], ", ") + fmt.Sprintf(", … (%d de plus)", len(list)-max)
}

var (
	infoZipVersion  = regexp.MustCompile(`This is Zip (\d+)\.(\d+)`)
	sevenZipVersion = regexp.MustCompile(`7-Zip[^\r\n]*?(\d+)\.(\d+)`)
)

// CheckZip vérifie tools.zip (CheckTool) puis sa version : Info-ZIP 3.0 ou plus, 7-Zip
// 16.02 ou plus (-snl, -spd). Renvoie la version lue.
func CheckZip(ctx context.Context, r Runner, path string) (string, error) {
	if err := CheckTool(path); err != nil {
		return "", err
	}
	flavor := config.ZipFlavor(path)
	var args []string
	re, minMajor, minMinor, minText := infoZipVersion, 3, 0, "3.0"
	switch flavor {
	case config.ZipInfoZIP:
		if runtime.GOOS == "windows" {
			// Info-ZIP pour Windows n'est plus maintenu : noms lus dans la page de code
			// ANSI, liens non gérés.
			return "", fmt.Errorf("%s : sous Windows, utiliser 7-Zip (7z.exe)", path)
		}
		args = []string{"-v"}
	case config.Zip7Zip:
		re, minMajor, minMinor, minText = sevenZipVersion, 16, 2, "16.02"
	default:
		return "", fmt.Errorf("%q n'est ni zip (Info-ZIP) ni 7z (7-Zip)", path)
	}
	var out strings.Builder
	tmp := os.TempDir()
	if _, err := r.RunSpec(ctx, execx.Spec{Name: path, Args: args, Env: toolEnv(tmp), Out: &out}); err != nil {
		return "", fmt.Errorf("lancement de %s : %w", path, err)
	}
	m := re.FindStringSubmatch(out.String())
	if m == nil {
		return "", fmt.Errorf("%s : version illisible (%s attendu)", path, flavor)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	v := m[1] + "." + m[2]
	if major < minMajor || major == minMajor && minor < minMinor {
		return v, fmt.Errorf("%s : version %s trop ancienne (%s ou plus récente attendue)", path, v, minText)
	}
	return v, nil
}
