package restore

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type memBucket struct {
	mu      sync.Mutex
	data    map[string][]byte // versionID → contenu
	badSize map[string]bool
	badMD5  map[string]bool
	gets    int
}

func (m *memBucket) GetVersion(_ context.Context, key, id string) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	c, ok := m.data[id]
	if !ok {
		return Object{}, errors.New("NoSuchVersion")
	}
	sum := md5.Sum(c)
	body := append([]byte(nil), c...)
	if m.badSize[id] {
		body = body[:len(body)-1]
	}
	if m.badMD5[id] {
		body[0] ^= 0xff
	}
	return Object{Body: io.NopCloser(bytes.NewReader(body)), Size: int64(len(c)), MD5: hex.EncodeToString(sum[:])}, nil
}

func newDest(t *testing.T) *Dest {
	t.Helper()
	d, err := OpenDest(t.TempDir(), Owner{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestDestWrite(t *testing.T) {
	d := newDest(t)
	when := time.Date(2026, 10, 20, 22, 30, 0, 0, time.UTC)
	m := &memBucket{data: map[string][]byte{"v1": []byte("bonjour"), "v2": []byte("x")}}
	v := Version{Key: "k/a.txt", VersionID: "v1", Size: 7, LastModified: when}
	if err := d.Write(context.Background(), m, "Documents/sous dossier/a.txt", v); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d.Dir, "Documents", "sous dossier", "a.txt")
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "bonjour" {
		t.Fatalf("%q %v", got, err)
	}
	fi, _ := os.Stat(p)
	if !fi.ModTime().Equal(when) {
		t.Errorf("date %v, attendu %v (date de la sauvegarde)", fi.ModTime(), when)
	}
	if !d.Done("Documents/sous dossier/a.txt", 7) || d.Done("Documents/sous dossier/a.txt", 8) {
		t.Error("Done")
	}
	// jamais d'écrasement
	if err := d.Write(context.Background(), m, "Documents/sous dossier/a.txt", Version{Key: "k", VersionID: "v2", Size: 1}); !errors.Is(err, ErrExists) {
		t.Errorf("écrasement : %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "bonjour" {
		t.Error("fichier modifié")
	}
	// aucun fichier temporaire laissé
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("restes : %v", entries)
	}
}

func TestDestWriteAltered(t *testing.T) {
	d := newDest(t)
	m := &memBucket{data: map[string][]byte{"v1": []byte("bonjour")}, badSize: map[string]bool{"v1": true}}
	v := Version{Key: "k", VersionID: "v1", Size: 7}
	if err := d.Write(context.Background(), m, "a.txt", v); err == nil || !strings.Contains(err.Error(), "taille") {
		t.Fatalf("taille : %v", err)
	}
	m.badSize = nil
	m.badMD5 = map[string]bool{"v1": true}
	if err := d.Write(context.Background(), m, "a.txt", v); err == nil || !strings.Contains(err.Error(), "empreinte") {
		t.Fatalf("empreinte : %v", err)
	}
	entries, _ := os.ReadDir(d.Dir)
	if len(entries) != 0 {
		t.Errorf("fichier laissé après un échec : %v", entries)
	}
	if err := d.Write(context.Background(), m, "../evasion.txt", v); err == nil {
		t.Fatal("nom dangereux accepté")
	}
}

func TestDestRefusesLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("liens symboliques : droits particuliers sous Windows")
	}
	outside := t.TempDir()
	d := newDest(t)
	if err := os.Symlink(outside, filepath.Join(d.Dir, "Documents")); err != nil {
		t.Fatal(err)
	}
	m := &memBucket{data: map[string][]byte{"v1": []byte("bonjour")}}
	err := d.Write(context.Background(), m, "Documents/a.txt", Version{Key: "k", VersionID: "v1", Size: 7})
	if !errors.Is(err, ErrLink) {
		t.Fatalf("lien suivi : %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("écrit hors de la destination : %v", entries)
	}
	// un lien à la place du fichier lui-même
	if err := os.Symlink(filepath.Join(outside, "cible"), filepath.Join(d.Dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := d.Write(context.Background(), m, "b.txt", Version{Key: "k", VersionID: "v1", Size: 7}); !errors.Is(err, ErrLink) {
		t.Fatalf("lien remplacé : %v", err)
	}
	// la destination elle-même ne peut pas être un lien
	link := filepath.Join(t.TempDir(), "lien")
	os.Symlink(outside, link)
	if _, err := OpenDest(link, Owner{}); err == nil {
		t.Fatal("destination lien acceptée")
	}
}

func TestMarkConflicts(t *testing.T) {
	items := []Item{{Out: "R/a.txt"}, {Out: "R/A.TXT"}, {Out: "R/x"}, {Out: "R/x/y"}, {Out: "R/d/e"}, {Out: "R/D"}, {Out: "R/nul.txt"}}
	MarkConflicts(items, true)
	var bad []string
	for _, it := range items {
		if it.Problem != "" {
			bad = append(bad, it.Out)
		}
	}
	if strings.Join(bad, ",") != "R/A.TXT,R/x/y,R/D,R/nul.txt" {
		t.Fatalf("conflits : %v", bad)
	}
	items = []Item{{Out: "R/a.txt"}, {Out: "R/A.TXT"}}
	MarkConflicts(items, false)
	if items[1].Problem != "" {
		t.Fatal("casse respectée : pas de conflit")
	}
}

func TestRestoreParallel(t *testing.T) {
	d := newDest(t)
	m := &memBucket{data: map[string][]byte{}}
	var items []Item
	for i := 0; i < 50; i++ {
		id := "v" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		m.data[id] = []byte(strings.Repeat(id, i+1))
		items = append(items, Item{File: File{Version: Version{Key: "k" + id, VersionID: id, Size: int64(3 * (i + 1))}}, Out: "d" + id[1:2] + "/" + id + ".bin"})
	}
	items = append(items, Item{File: File{Problem: "nom refusé"}, Out: "x"})
	var ok, refused []string
	Restore(context.Background(), m, d, items, 8, nil, func(o Outcome) {
		switch {
		case o.Refused:
			refused = append(refused, o.Item.Out)
		case o.Err == nil:
			ok = append(ok, o.Item.Out)
		default:
			t.Errorf("%s : %v", o.Item.Out, o.Err)
		}
	})
	if len(ok) != 50 || len(refused) != 1 {
		t.Fatalf("%d restaurés, %d refusés", len(ok), len(refused))
	}

	// reprise : tout est déjà là
	resumed := map[string]string{}
	for _, it := range items[:50] {
		resumed[it.Out] = it.Version.VersionID
	}
	m.gets = 0
	var skipped int
	Restore(context.Background(), m, d, items[:50], 4, resumed, func(o Outcome) {
		if o.Skipped {
			skipped++
		}
	})
	if skipped != 50 || m.gets != 0 {
		t.Fatalf("reprise : %d sautés, %d lectures", skipped, m.gets)
	}

	// annulation : plus rien n'est lu
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d2 := newDest(t)
	var errs int
	m.gets = 0
	Restore(ctx, m, d2, items[:10], 2, nil, func(o Outcome) {
		if o.Err != nil {
			errs++
		}
	})
	if errs != 10 || m.gets != 0 {
		t.Fatalf("annulé : %d erreurs, %d lectures", errs, m.gets)
	}
}

func TestMarker(t *testing.T) {
	d := newDest(t)
	at := time.Date(2026, 10, 20, 23, 59, 59, 0, time.UTC)
	m := Marker{Targets: []string{"/home/alice/Documents"}, At: &at, Started: time.Now()}
	p, err := d.StartProgress(m)
	if err != nil {
		t.Fatal(err)
	}
	p.Done(Item{File: File{Version: Version{VersionID: "v1"}}, Out: "Documents/a b.txt"})
	p.Finish(false)
	got, done, err := ReadMarker(d.Dir)
	if err != nil || got == nil || !got.Same(m) || done["Documents/a b.txt"] != "v1" {
		t.Fatalf("%+v %v %v", got, done, err)
	}
	other := m
	other.At = nil
	if got.Same(other) {
		t.Fatal("dates différentes")
	}
	p, _ = d.StartProgress(m)
	p.Finish(true)
	if got, _, _ := ReadMarker(d.Dir); got != nil {
		t.Fatal("reprise non effacée")
	}
	if names, _ := os.ReadDir(d.Dir); len(names) != 0 {
		var n []string
		for _, e := range names {
			n = append(n, e.Name())
		}
		sort.Strings(n)
		t.Fatalf("restes : %v", n)
	}
}
