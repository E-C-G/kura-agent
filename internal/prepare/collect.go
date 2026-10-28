package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/E-C-G/kura-agent/internal/guard"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// collected : ce que copySource a retenu, laissé de côté ou refusé.
type collected struct {
	names   []string // fichiers copiés (chemins relatifs, séparateurs /)
	missing []string // illisibles (verrouillés, refusés) : manquants
	refused []string // noms que l'outil ne recevrait pas tels quels : manquants
	links   []string // liens et jonctions, jamais suivis
	ignored []string // ni fichiers ni dossiers (FIFO, périphériques…)
	agent   []string // dossiers ou fichiers de l'agent laissés de côté
	bytes   int64
}

// copySource copie dans dst les fichiers retenus de src : exclusions appliquées, liens et
// jonctions jamais suivis, dossiers de l'agent laissés de côté. Chaque fichier est ouvert
// sous la source (os.Root) : un lien posé pendant la copie ne mène jamais hors d'elle.
// L'outil d'archivage ne lit ensuite que cette copie, dans la zone de préparation que
// seul l'agent peut modifier : il ne suit donc aucun lien posé par un utilisateur, et ne
// charge rien d'un dossier qu'un utilisateur contrôle.
func copySource(ctx context.Context, src string, p config.Prepare, agentPaths []string, flavor, dst string) (collected, error) {
	var c collected
	if err := guard.CheckSourceNotLink(src); err != nil {
		return c, err
	}
	agent, agentNames := agentFiles(agentPaths)
	for dir := filepath.Clean(src); ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(dir); err == nil && sameAny(fi, agent) {
			return c, fmt.Errorf("%s : la source est un dossier de l'agent ou se trouve dessous", src)
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	before, err := os.Lstat(src)
	if err != nil {
		return c, err
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		return c, err
	}
	defer root.Close()
	if now, err := root.Stat("."); err != nil || !os.SameFile(before, now) {
		return c, fmt.Errorf("%s a changé pendant l'ouverture (remplacé par un lien ?)", src)
	}
	excluded := guard.Excluder(p.Exclude)
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if rel == "." {
				return err
			}
			c.missing = append(c.missing, rel+"/")
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if rel == "." {
			return nil
		}
		skip := func() error {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if agentNames[strings.ToLower(d.Name())] {
			if fi, err := os.Stat(filepath.Join(src, filepath.FromSlash(rel))); err == nil && sameAny(fi, agent) {
				c.agent = append(c.agent, rel)
				return skip()
			}
		}
		if excluded(rel, d.IsDir()) {
			return skip()
		}
		t := d.Type()
		if t&fs.ModeSymlink != 0 || t&fs.ModeIrregular != 0 && d.IsDir() {
			c.links = append(c.links, rel)
			return skip()
		}
		if d.IsDir() {
			return nil
		}
		if t&fs.ModeIrregular == 0 && !t.IsRegular() {
			c.ignored = append(c.ignored, rel)
			return nil
		}
		if badName(rel, flavor) {
			c.refused = append(c.refused, fmt.Sprintf("%q", rel))
			return nil
		}
		switch n, err := copyOne(ctx, root, rel, dst); {
		case errors.Is(err, errNotRegular):
			c.ignored = append(c.ignored, rel)
		case errors.Is(err, errEscapes):
			c.links = append(c.links, rel)
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.missing = append(c.missing, rel)
		default:
			c.names = append(c.names, rel)
			c.bytes += n
		}
		return nil
	})
	return c, err
}

var (
	errNotRegular = errors.New("pas un fichier ordinaire")
	errEscapes    = errors.New("lien hors de la source")
)

// copyOne copie rel (ouvert sous root) vers dst/rel, avec sa date de modification.
func copyOne(ctx context.Context, root *os.Root, rel, dst string) (int64, error) {
	in, err := root.OpenFile(rel, os.O_RDONLY|openNoBlock, 0)
	if err != nil {
		if strings.Contains(err.Error(), "escapes") {
			return 0, errEscapes
		}
		return 0, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, errNotRegular
	}
	target := filepath.Join(dst, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := copyCtx(ctx, out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(target, fi.ModTime(), fi.ModTime())
	}
	if err != nil {
		os.Remove(target)
		return 0, err
	}
	return n, nil
}

// copyCtx copie par blocs en s'arrêtant si ctx est annulé (durée maximale dépassée).
func copyCtx(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		k, rerr := r.Read(buf)
		if k > 0 {
			if _, err := w.Write(buf[:k]); err != nil {
				return n, err
			}
			n += int64(k)
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// badName : nom que l'outil ne recevrait pas tel quel. Une ligne par nom (retour à la
// ligne impossible) ; 7-Zip retire les espaces au bord d'une ligne de sa liste et les
// guillemets qui l'entourent ; Windows ignore le point ou l'espace qui finit un nom.
func badName(rel, flavor string) bool {
	if strings.ContainsAny(rel, "\r\n") {
		return true
	}
	if flavor == config.Zip7Zip && (strings.TrimSpace(rel) != rel || strings.HasPrefix(rel, `"`)) {
		return true
	}
	if runtime.GOOS == "windows" {
		for _, part := range strings.Split(rel, "/") {
			if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
				return true
			}
		}
	}
	return false
}

// agentFiles : identité des dossiers et fichiers de l'agent (comparés par fichier, pas par
// nom : un nom court 8.3 ou un autre chemin vers le même dossier ne les contourne pas), et
// leurs noms en minuscules pour un premier tri rapide.
func agentFiles(paths []string) ([]os.FileInfo, map[string]bool) {
	var infos []os.FileInfo
	names := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil {
			infos = append(infos, fi)
			names[strings.ToLower(fi.Name())] = true
		}
	}
	return infos, names
}

func sameAny(fi os.FileInfo, list []os.FileInfo) bool {
	for _, a := range list {
		if os.SameFile(fi, a) {
			return true
		}
	}
	return false
}
