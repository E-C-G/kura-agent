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
	// Links : liens symboliques et jonctions rencontrés (hors exclusions), chemins
	// relatifs à la source en séparateurs /. Jamais suivis par le comptage ; à exclure
	// de aws s3 sync, qui suit les jonctions sous Windows malgré --no-follow-symlinks.
	Links []string
	// Paths : fichiers retenus (chemins relatifs, séparateurs /), seulement avec List.
	Paths []string
	// Skipped : dossiers laissés de côté parce qu'ils appartiennent à l'agent (List).
	Skipped []string
}

// CountFiles compte les fichiers de root en appliquant les mêmes exclusions que
// `aws s3 sync --exclude`. Les éléments illisibles (ex. System Volume Information)
// sont comptés à part et ne font pas échouer le comptage.
// Une source absente ou illisible à la racine est une erreur.
func CountFiles(root string, excludes []string) (Census, error) { return Scan(root, excludes) }

// Scan parcourt la source sans suivre aucun lien : fichiers comptés (garde-fou) et liens
// ou jonctions relevés (Census.Links).
func Scan(root string, excludes []string) (Census, error) { return walk(root, excludes, false, nil) }

// List parcourt la source comme Scan et relève en plus chaque fichier retenu
// (Census.Paths), pour le confier à un outil (archive zip) qui ne doit lire qu'eux. Les
// dossiers de skip (ceux de l'agent : données, coffre…) ne sont jamais parcourus.
func List(root string, excludes, skip []string) (Census, error) {
	return walk(root, excludes, true, skip)
}

func walk(root string, excludes []string, collect bool, skip []string) (Census, error) {
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
		if d.IsDir() && underAny(p, skip) {
			c.Skipped = append(c.Skipped, rel)
			return fs.SkipDir
		}
		if excluded(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			// Lien symbolique, jonction (ou autre point de reparse : fichier à la
			// demande de OneDrive, déduplication), vérifié par son étiquette. Si
			// l'étiquette ne se lit pas (accès refusé…), traité comme un lien : exclu.
			if link, err := isLink(p); err != nil || link {
				c.Links = append(c.Links, rel)
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil // jamais compté ni parcouru
			}
		}
		if !d.IsDir() { // fichiers, fichiers de substitution cloud…
			c.Files++
			if collect {
				c.Paths = append(c.Paths, rel)
			}
		}
		return nil
	})
	if errors.Is(err, rootErr) {
		return c, fmt.Errorf("source %q illisible", root)
	}
	return c, err
}

// underAny : p est l'un des dossiers dirs ou se trouve dessous (casse ignorée sous
// Windows : filepath.Rel y compare les noms sans tenir compte de la casse).
func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(d), filepath.Clean(p))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
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

// Kind dit quel seuil a bloqué le job.
type Kind int

const (
	Passed      Kind = iota
	TooFewFiles      // source vide ou démontée : jamais levé par --force-deletes
	TooManyDeletes
	DeleteRatio
)

// Decision est le verdict du garde-fou pour un job.
type Decision struct {
	Blocked bool
	Kind    Kind
	Reason  string
}

// Evaluate applique les seuils du garde-fou.
// sourceFiles : fichiers de la source ; plannedDeletes : suppressions annoncées par --dryrun.
func Evaluate(g config.Guard, sourceFiles, plannedDeletes int) Decision {
	if sourceFiles < g.MinSourceFiles {
		return Decision{true, TooFewFiles, fmt.Sprintf(
			"la source ne contient que %d fichier(s), moins que le minimum de %d : source vide ou démontée ?",
			sourceFiles, g.MinSourceFiles)}
	}
	if g.MaxDeletes > 0 && plannedDeletes > g.MaxDeletes {
		return Decision{true, TooManyDeletes, fmt.Sprintf(
			"%d suppressions prévues dans le bucket, plus que le maximum de %d", plannedDeletes, g.MaxDeletes)}
	}
	if g.MaxDeleteRatio > 0 && plannedDeletes > 0 {
		ratio := float64(plannedDeletes) / float64(sourceFiles+plannedDeletes)
		if ratio > g.MaxDeleteRatio {
			return Decision{true, DeleteRatio, fmt.Sprintf(
				"%d suppressions prévues, soit %.1f %% des fichiers, plus que le maximum de %.1f %%",
				plannedDeletes, ratio*100, g.MaxDeleteRatio*100)}
		}
	}
	return Decision{}
}
