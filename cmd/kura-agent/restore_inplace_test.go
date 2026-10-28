package main

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (b *toBench) local(rel, content string) string {
	p := filepath.Join(b.src, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(content), 0o644)
	return p
}

func TestRestoreInPlace(t *testing.T) {
	b := newToBench(t)
	// a.txt présent et modifié, b.txt supprimé par erreur
	b.local("Documents/a.txt", "modifié depuis")
	docs := filepath.Join(b.src, "Documents")

	// --list : le plan, sans verrou ni écriture
	code, out, _ := b.run(docs, "--in-place", "--list")
	if code != 0 || b.locked != 0 || !strings.Contains(out, "à remettre (absents)       : 1") || !strings.Contains(out, "laissés (présents)         : 1") {
		t.Fatalf("%d %d %s", code, b.locked, out)
	}
	if _, err := os.Stat(filepath.Join(docs, "sous dossier", "b.txt")); err == nil {
		t.Fatal("--list a écrit")
	}

	code, out, stderr := b.run(docs, "--in-place", "--yes")
	if code != 0 || b.locked != 1 || b.held {
		t.Fatalf("%d verrou %d/%v\n%s\n%s", code, b.locked, b.held, out, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(docs, "sous dossier", "b.txt")); string(got) != want("b1", 5) {
		t.Errorf("b.txt non remis : %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(docs, "a.txt")); string(got) != "modifié depuis" {
		t.Errorf("a.txt touché sans --replace : %q", got)
	}
	if !strings.Contains(out, "1 fichier(s) restauré(s)") || !strings.Contains(out, "en place") {
		t.Errorf("%s", out)
	}
	st := b.reports[len(b.reports)-1]
	if st.Mode != "in_place" || st.Files != 1 || st.Skipped != 1 || st.To != "en place" {
		t.Errorf("bilan : %+v", st)
	}

	// relancée : tout est là, rien à faire
	code, out, _ = b.run(docs, "--in-place", "--yes")
	if code != 0 || !strings.Contains(out, "Rien à remettre en place") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestRestoreInPlaceReplace(t *testing.T) {
	b := newToBench(t)
	docs := filepath.Join(b.src, "Documents")
	b.local("Documents/a.txt", "modifié depuis")
	b.local("Documents/sous dossier/b.txt", want("b1", 5)) // identique
	b.l.versions[1].ETag = etagOf(want("a2", 12))
	b.l.versions[2].ETag = etagOf(want("b1", 5))
	code, out, stderr := b.run(docs, "--in-place", "--replace", "--yes")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(docs, "a.txt")); string(got) != want("a2", 12) {
		t.Errorf("a.txt non remplacé : %q", got)
	}
	kept, _ := filepath.Glob(filepath.Join(docs, "a.txt.avant-restauration-*"))
	if len(kept) != 1 {
		t.Fatalf("copie de l'actuel : %v", kept)
	}
	if got, _ := os.ReadFile(kept[0]); string(got) != "modifié depuis" {
		t.Errorf("copie : %q", got)
	}
	if !strings.Contains(out, "dont 1 remplacé(s)") {
		t.Errorf("%s", out)
	}
	// b.txt identique : ni relu ni remplacé
	for _, g := range b.l.gets {
		if g == "b1" {
			t.Error("fichier identique relu")
		}
	}
	logs, _ := filepath.Glob(filepath.Join(b.dir, "logs", "restore-*.log"))
	lj, _ := os.ReadFile(logs[0])
	for _, s := range []string{"REMPLACÉ\t", "IDENTIQUE\t", "actuel gardé : "} {
		if !strings.Contains(string(lj), s) {
			t.Errorf("journal sans %q :\n%s", s, lj)
		}
	}
	if st := b.reports[0]; st.Mode != "replace" || st.Replaced != 1 || st.Files != 1 {
		t.Errorf("%+v", st)
	}
}

func TestRestoreInPlaceRefusals(t *testing.T) {
	b := newToBench(t)
	docs := filepath.Join(b.src, "Documents")
	for _, args := range [][]string{
		{docs, "--in-place", "--to", filepath.Join(b.dir, "R")},
		{docs, "--replace", "--yes"},
		{docs, "--versions", "--in-place"},
	} {
		if code, _, stderr := b.run(args...); code != exitRestoreRefused || !strings.Contains(stderr, "Refusé") {
			t.Errorf("%q : %d %q", args, code, stderr)
		}
	}
	b.busy = true
	if code, _, stderr := b.run(docs, "--in-place", "--yes"); code != exitRestoreRefused || !strings.Contains(stderr, "sauvegarde est en cours") {
		t.Fatalf("verrou : %d %q", code, stderr)
	}
	if len(b.l.prefixes) != 0 {
		t.Fatal("bucket lu sans le verrou")
	}
	b.busy = false
	// source du job absente : --to conseillé
	os.RemoveAll(b.src)
	if code, _, stderr := b.run(docs, "--in-place", "--yes"); code != exitRestoreRefused || !strings.Contains(stderr, "--to") || b.held {
		t.Fatalf("source absente : %d %q", code, stderr)
	}
}

func etagOf(s string) string { return `"` + md5hex([]byte(s)) + `"` }

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}
