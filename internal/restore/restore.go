// Package restore retrouve dans le bucket l'état d'un fichier ou d'un dossier à une
// date : versions conservées par le versioning, marqueurs de suppression, et choix,
// fichier par fichier, de la version en place à cette date.
//
// Lot 1 : lecture seule (historique d'un fichier, inventaire d'un dossier à une date).
// Le paquet n'écrit jamais dans le bucket : son interface de stockage ne fait que
// lister.
package restore

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/pkg/config"
)

// Version est une version d'un objet, ou un marqueur de suppression.
type Version struct {
	Key          string
	VersionID    string
	LastModified time.Time
	Size         int64
	Latest       bool // version actuelle de la clé
	Deleted      bool // marqueur de suppression
}

// Lister liste les versions et marqueurs de suppression sous un préfixe.
type Lister interface {
	ListVersions(ctx context.Context, prefix string) ([]Version, error)
}

// Target est ce qu'un chemin local désigne dans le bucket.
type Target struct {
	Job   config.Job
	Local string // chemin local demandé, nettoyé (séparateurs /)
	Key   string // clé du fichier, ou préfixe du dossier, sans / final
	Root  bool   // le chemin est la source entière du job
}

// ErrOutside : chemin hors des sources des jobs.
var ErrOutside = errors.New("chemin hors des sources des jobs de la configuration")

// normalize met un chemin en forme comparable : séparateurs /, sans / final.
func normalize(p string, windows bool) string {
	if windows {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	p = path.Clean(p)
	if p == "." {
		return ""
	}
	return p
}

// Resolve rattache un chemin local au job dont la source le contient (la plus
// précise si des sources s'imbriquent) et calcule sa clé dans le bucket :
// destination du job + chemin relatif. Sous Windows, la comparaison ignore la casse.
func Resolve(cfg *config.Config, local string, windows bool) (Target, error) {
	p := normalize(local, windows)
	if p == "" {
		return Target{}, fmt.Errorf("chemin vide")
	}
	same := func(a, b string) bool {
		if windows {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	var best *config.Job
	var bestSrc string
	for i := range cfg.Jobs {
		j := &cfg.Jobs[i]
		src := normalize(j.Source, windows)
		if src == "" || j.Unavailable != "" {
			continue
		}
		inside := same(p, src)
		if !inside && len(p) > len(src) && p[len(src)] == '/' && same(p[:len(src)], src) {
			inside = true
		}
		if src == "/" && strings.HasPrefix(p, "/") {
			inside = true
		}
		if inside && len(src) > len(bestSrc) {
			best, bestSrc = j, src
		}
	}
	if best == nil {
		return Target{}, fmt.Errorf("%s : %w", local, ErrOutside)
	}
	dest := strings.Trim(best.Destination, "/")
	t := Target{Job: *best, Local: p, Key: dest, Root: same(p, bestSrc)}
	if !t.Root {
		rel := strings.TrimPrefix(p[len(bestSrc):], "/")
		if dest == "" {
			t.Key = rel
		} else {
			t.Key = dest + "/" + rel
		}
	}
	return t, nil
}

// Prefix : préfixe à lister pour la cible (dossier ou fichier).
func (t Target) Prefix() string {
	if t.Root && t.Key == "" {
		return ""
	}
	return t.Key
}

// Covers : la clé relève de la cible (le fichier lui-même, ou ce qui est dessous).
func (t Target) Covers(key string, foldCase bool) bool {
	if t.Key == "" {
		return true
	}
	eq := func(a, b string) bool {
		if foldCase {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	if eq(key, t.Key) {
		return !t.Root
	}
	return len(key) > len(t.Key) && key[len(t.Key)] == '/' && eq(key[:len(t.Key)], t.Key)
}

// File est un fichier dans l'état demandé.
type File struct {
	Key     string
	Rel     string  // chemin sous la cible (nom du fichier pour une cible fichier)
	Version Version // version retenue
	// DeletedSince : supprimé sur le poste après la date demandée (la version actuelle
	// est un marqueur de suppression) ; nil sinon.
	DeletedSince *time.Time
	// NewerSince : une version plus récente existe (le fichier a changé depuis).
	NewerSince bool
	// Problem : nom inutilisable pour une restauration (vide si aucun).
	Problem string
}

// byKey regroupe les versions par clé, de la plus récente à la plus ancienne (à date
// égale, la version actuelle d'abord).
func byKey(versions []Version) map[string][]Version {
	m := map[string][]Version{}
	for _, v := range versions {
		m[v.Key] = append(m[v.Key], v)
	}
	for _, vs := range m {
		sort.SliceStable(vs, func(i, j int) bool {
			if !vs[i].LastModified.Equal(vs[j].LastModified) {
				return vs[i].LastModified.After(vs[j].LastModified)
			}
			return vs[i].Latest && !vs[j].Latest
		})
	}
	return m
}

// StateAt renvoie les fichiers présents dans la cible à la date at (zéro : état de
// la dernière sauvegarde), triés par chemin. Pour chaque clé : la plus récente version
// enregistrée au plus tard à at ; un marqueur de suppression signifie « absent ».
func StateAt(versions []Version, t Target, at time.Time, foldCase bool) []File {
	var out []File
	for key, vs := range byKey(versions) {
		if !t.Covers(key, foldCase) {
			continue
		}
		idx := -1
		for i, v := range vs {
			if at.IsZero() {
				if v.Latest {
					idx = i
					break
				}
				continue
			}
			if !v.LastModified.After(at) {
				idx = i
				break
			}
		}
		if idx < 0 || vs[idx].Deleted {
			continue
		}
		f := File{Key: key, Version: vs[idx], NewerSince: idx > 0}
		if latest := vs[0]; latest.Deleted && idx > 0 {
			d := latest.LastModified
			f.DeletedSince = &d
		}
		if !t.Root && len(key) == len(t.Key) {
			f.Rel = path.Base(key) // la cible est ce fichier
		} else {
			f.Rel = strings.TrimPrefix(key[len(t.Key):], "/")
		}
		f.Problem = CheckRel(f.Rel)
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// History renvoie les versions d'une clé, de la plus récente à la plus ancienne.
func History(versions []Version, key string, foldCase bool) []Version {
	var out []Version
	for k, vs := range byKey(versions) {
		if k == key || (foldCase && strings.EqualFold(k, key)) {
			out = append(out, vs...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastModified.After(out[j].LastModified) })
	return out
}

// windowsReserved : noms de périphériques interdits sous Windows, extension comprise.
var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// CheckRel dit pourquoi un chemin relatif venu du bucket ne peut pas être restauré
// (vide si rien) : il ne doit jamais sortir de la destination ni viser un nom que
// Windows refuse. Règle commune aux trois systèmes : un bucket peut être restauré
// ailleurs que là où il a été sauvegardé.
func CheckRel(rel string) string {
	if rel == "" {
		return "nom vide"
	}
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) || strings.ContainsRune(rel, 0) {
		return "chemin absolu ou séparateur inattendu"
	}
	if len(rel) >= 2 && rel[1] == ':' {
		return "lecteur dans le nom"
	}
	for _, seg := range strings.Split(rel, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return "segment « " + seg + " » refusé"
		case strings.ContainsAny(seg, `<>:"|?*`):
			return "caractère interdit sous Windows"
		case strings.HasSuffix(seg, " ") || strings.HasSuffix(seg, "."):
			return "nom finissant par une espace ou un point"
		}
		base := strings.ToUpper(seg)
		if i := strings.IndexByte(base, '.'); i >= 0 {
			base = base[:i]
		}
		if windowsReserved[base] {
			return "nom réservé sous Windows (" + seg + ")"
		}
		for _, r := range seg {
			if r < 0x20 {
				return "caractère de contrôle dans le nom"
			}
		}
	}
	return ""
}

// Total additionne la taille des fichiers.
func Total(files []File) int64 {
	var n int64
	for _, f := range files {
		n += f.Version.Size
	}
	return n
}
