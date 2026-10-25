// Package guard empêche une synchronisation avec --delete de vider le bucket
// quand la source est vide, démontée ou massivement modifiée (rançongiciel).
package guard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/E-C-G/kura-agent/pkg/config"
)

// Census est le résultat du comptage d'une source.
type Census struct {
	Files      int // fichiers comptés (hors exclusions)
	Unreadable int // dossiers ou fichiers illisibles, ignorés
}

// CountFiles compte les fichiers de root en appliquant les mêmes exclusions que
// `aws s3 sync --exclude`. Les éléments illisibles (ex. System Volume Information)
// sont comptés à part et ne font pas échouer le comptage.
// Une source absente ou illisible à la racine est une erreur.
func CountFiles(root string, excludes []string) (Census, error) {
	var c Census
	st, err := os.Stat(root)
	if err != nil {
		return c, fmt.Errorf("source inaccessible : %w", err)
	}
	if !st.IsDir() {
		return c, fmt.Errorf("source %q : ce n'est pas un dossier", root)
	}
	matchers := make([]*regexp.Regexp, 0, len(excludes))
	for _, e := range excludes {
		matchers = append(matchers, globToRegexp(e))
	}
	excluded := func(rel string, dir bool) bool {
		// La CLI compare le motif au chemin complet (ex. E:\$RECYCLE.BIN\x) : on essaie le
		// chemin relatif avec et sans séparateur initial, en '/' et en '\', et pour un
		// dossier avec séparateur final, pour couvrir les motifs « *\$RECYCLE.BIN\* ».
		win := strings.ReplaceAll(rel, "/", `\`)
		cands := []string{rel, "/" + rel, win, `\` + win}
		if dir {
			cands = append(cands, rel+"/", "/"+rel+"/", win+`\`, `\`+win+`\`)
		}
		for _, m := range matchers {
			for _, cand := range cands {
				if m.MatchString(cand) {
					return true
				}
			}
		}
		return false
	}
	rootErr := errors.New("racine")
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return rootErr
			}
			c.Unreadable++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if excluded(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			c.Files++
		}
		return nil
	})
	if errors.Is(err, rootErr) {
		return c, fmt.Errorf("source %q illisible", root)
	}
	return c, err
}

// globToRegexp traduit un motif fnmatch (celui de --exclude) en expression régulière :
// '*' couvre n'importe quelle suite de caractères, séparateurs compris ; '?' un caractère.
func globToRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile("(?i)" + b.String()) // Windows : chemins insensibles à la casse
}

// Decision est le verdict du garde-fou pour un job.
type Decision struct {
	Blocked bool
	Reason  string
}

// Evaluate applique les seuils du garde-fou.
// sourceFiles : fichiers de la source ; plannedDeletes : suppressions annoncées par --dryrun.
func Evaluate(g config.Guard, sourceFiles, plannedDeletes int) Decision {
	if sourceFiles < g.MinSourceFiles {
		return Decision{true, fmt.Sprintf(
			"la source ne contient que %d fichier(s), moins que le minimum de %d : source vide ou démontée ?",
			sourceFiles, g.MinSourceFiles)}
	}
	if g.MaxDeletes > 0 && plannedDeletes > g.MaxDeletes {
		return Decision{true, fmt.Sprintf(
			"%d suppressions prévues dans le bucket, plus que le maximum de %d", plannedDeletes, g.MaxDeletes)}
	}
	if g.MaxDeleteRatio > 0 && plannedDeletes > 0 {
		ratio := float64(plannedDeletes) / float64(sourceFiles+plannedDeletes)
		if ratio > g.MaxDeleteRatio {
			return Decision{true, fmt.Sprintf(
				"%d suppressions prévues, soit %.1f %% des fichiers, plus que le maximum de %.1f %%",
				plannedDeletes, ratio*100, g.MaxDeleteRatio*100)}
		}
	}
	return Decision{}
}
