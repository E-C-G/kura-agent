package restore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// Item est un fichier à restaurer : sa version et sa place sous la destination.
type Item struct {
	File
	Out string // chemin sous la destination (séparateurs /)
}

// MarkConflicts signale les fichiers qui ne peuvent pas coexister dans la destination :
// deux noms qui ne diffèrent que par la casse sur un système qui l'ignore (Windows,
// macOS), ou un fichier à la place d'un dossier. Le premier dans l'ordre est gardé.
func MarkConflicts(items []Item, foldCase bool) {
	norm := func(s string) string {
		if foldCase {
			return strings.ToLower(s)
		}
		return s
	}
	files := map[string]string{}
	dirs := map[string]string{}
	for i := range items {
		it := &items[i]
		if it.Problem != "" {
			continue
		}
		if p := CheckRel(it.Out); p != "" {
			it.Problem = p
			continue
		}
		k := norm(it.Out)
		if other, ok := files[k]; ok {
			it.Problem = "même nom que " + other + " à la casse près"
			continue
		}
		if other, ok := dirs[k]; ok {
			it.Problem = "nom déjà pris par le dossier de " + other
			continue
		}
		clash := ""
		for d := path.Dir(it.Out); d != "."; d = path.Dir(d) {
			if other, ok := files[norm(d)]; ok {
				clash = other
				break
			}
		}
		if clash != "" {
			it.Problem = "dossier à la place du fichier " + clash
			continue
		}
		files[k] = it.Out
		for d := path.Dir(it.Out); d != "."; d = path.Dir(d) {
			if _, ok := dirs[norm(d)]; !ok {
				dirs[norm(d)] = it.Out
			}
		}
	}
}

// Outcome est le résultat d'un fichier.
type Outcome struct {
	Item    Item
	Skipped bool  // déjà restauré (reprise)
	Refused bool  // nom refusé (Problem) : jamais écrit
	Err     error // nil : restauré
}

// Restore lit et écrit les fichiers, workers à la fois. done est appelé pour chaque
// fichier, depuis un seul goroutine à la fois. Les fichiers déjà présents avec la bonne
// taille et notés au journal de reprise sont sautés.
func Restore(ctx context.Context, g Getter, d *Dest, items []Item, workers int, resumed map[string]string, done func(Outcome)) {
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan Item)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				var o Outcome
				o.Item = it
				if v, listed := resumed[it.Out]; resumed != nil && (!listed || v == it.Version.VersionID) && d.Done(it.Out, it.Version.Size) {
					// noté au journal de reprise, ou renommé juste avant l'interruption
					o.Skipped = true
				} else if err := ctx.Err(); err != nil {
					o.Err = err
				} else {
					o.Err = d.Write(ctx, g, it.Out, it.Version)
				}
				mu.Lock()
				done(o)
				mu.Unlock()
			}
		}()
	}
	for _, it := range items {
		if it.Problem != "" {
			mu.Lock()
			done(Outcome{Item: it, Refused: true, Err: errors.New(it.Problem)})
			mu.Unlock()
			continue
		}
		jobs <- it
	}
	close(jobs)
	wg.Wait()
}

// Reprise : deux fichiers à la racine de la destination tant qu'une restauration n'est
// pas allée au bout. Le premier décrit la commande, le second reçoit une ligne par
// fichier restauré (version, chemin). Relancée à l'identique, la restauration saute ces
// fichiers ; terminée sans échec, elle efface les deux.
const (
	MarkerName   = ".kura-restore.json"
	ProgressName = ".kura-restore.done"
)

// Marker identifie une restauration : relancer la même reprend, une autre est refusée.
type Marker struct {
	Targets []string   `json:"targets"`
	At      *time.Time `json:"at,omitempty"`
	Started time.Time  `json:"started"`
}

// Same : même chemins et même date.
func (m Marker) Same(o Marker) bool {
	if strings.Join(m.Targets, "\n") != strings.Join(o.Targets, "\n") {
		return false
	}
	if (m.At == nil) != (o.At == nil) {
		return false
	}
	return m.At == nil || m.At.Equal(*o.At)
}

// ReadMarker lit la reprise d'une destination ; nil si aucune.
func ReadMarker(dir string) (*Marker, map[string]string, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()
	data, err := r.ReadFile(MarkerName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, fmt.Errorf("%s illisible : %w", MarkerName, err)
	}
	done := map[string]string{}
	if f, err := r.Open(ProgressName); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, p, ok := strings.Cut(sc.Text(), "\t"); ok {
				done[p] = v
			}
		}
		f.Close()
	}
	return &m, done, nil
}

// Progress note les fichiers restaurés dans la destination.
type Progress struct {
	d *Dest
	f *os.File
}

// StartProgress écrit (ou garde) la description de la restauration et ouvre la liste
// des fichiers restaurés en ajout.
func (d *Dest) StartProgress(m Marker) (*Progress, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := d.root.WriteFile(MarkerName, data, 0o644); err != nil {
		return nil, err
	}
	f, err := d.root.OpenFile(ProgressName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Progress{d: d, f: f}, nil
}

// Done note un fichier restauré.
func (p *Progress) Done(it Item) {
	fmt.Fprintf(p.f, "%s\t%s\n", it.Version.VersionID, it.Out)
}

// Finish ferme la liste ; complete efface la reprise.
func (p *Progress) Finish(complete bool) {
	p.f.Close()
	if complete {
		_ = p.d.root.Remove(ProgressName)
		_ = p.d.root.Remove(MarkerName)
	}
}
