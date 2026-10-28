package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/restore"
	"github.com/E-C-G/kura-agent/pkg/config"
)

type fakeLister struct {
	versions []restore.Version
	prefixes []string
	err      error
}

func (f *fakeLister) ListVersions(_ context.Context, prefix string) ([]restore.Version, error) {
	f.prefixes = append(f.prefixes, prefix)
	if f.err != nil {
		return nil, f.err
	}
	var out []restore.Version
	for _, v := range f.versions {
		if strings.HasPrefix(v.Key, prefix) {
			out = append(out, v)
		}
	}
	return out, nil
}

var restoreNow = time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)

func restoreFixture() (*fakeLister, restoreDeps, *bool) {
	at := func(d int) time.Time { return restoreNow.AddDate(0, 0, d) }
	l := &fakeLister{versions: []restore.Version{
		{Key: "acme-portable01/documents/Factures/2026.pdf", VersionID: "f1", LastModified: at(-20), Size: 1000},
		{Key: "acme-portable01/documents/Factures/2026.pdf", VersionID: "f2", LastModified: at(-2), Size: 1500, Latest: true},
		{Key: "acme-portable01/documents/Notes.txt", VersionID: "n1", LastModified: at(-30), Size: 10},
		{Key: "acme-portable01/documents/Notes.txt", VersionID: "nx", LastModified: at(-1), Deleted: true, Latest: true},
		{Key: "acme-portable01/projets/Plan.docx", VersionID: "p1", LastModified: at(-5), Size: 2048, Latest: true},
	}}
	admin := true
	cfg := &config.Config{Jobs: []config.Job{
		{ID: "documents", Source: `C:\Users\alice\Documents`, Destination: "acme-portable01/documents"},
		{ID: "projets", Source: `D:\Projets`, Destination: "acme-portable01/projets"},
	}}
	d := restoreDeps{
		checkAdmin: func() error {
			if !admin {
				return errors.New("console d'administration requise")
			}
			return nil
		},
		loadConfig: func(string) (*config.Config, error) { return cfg, nil },
		bucket:     func(*config.Config) (restore.Lister, error) { return l, nil },
		now:        func() time.Time { return restoreNow },
		windows:    true,
	}
	return l, d, &admin
}

func restoreRun(d restoreDeps, args ...string) (int, string, string) {
	var out, errOut strings.Builder
	code := runRestore(args, &out, &errOut, d)
	return code, out.String(), errOut.String()
}

func TestRestoreRefusals(t *testing.T) {
	_, d, admin := restoreFixture()
	cases := [][]string{
		{},
		{"--list"},
		{`C:\a`, `C:\b`, "--list"},
		{`C:\a`, "--all", "--list"},
		{`C:\a`, "--versions", "--list"},
		{`C:\a`, "--versions", "--at", "2026-10-01"},
		{`C:\a`},
		{`C:\a`, "--to", `E:\Restauration`},
		{`C:\a`, "--in-place"},
		{`C:\a`, "--list", "--at", "hier"},
	}
	for _, args := range cases {
		if code, _, stderr := restoreRun(d, args...); code != exitRestoreRefused || !strings.Contains(stderr, "Refusé") {
			t.Errorf("%q : code %d, %q", args, code, stderr)
		}
	}
	*admin = false
	if code, _, stderr := restoreRun(d, `C:\Users\alice\Documents`, "--list"); code != exitRestoreRefused || !strings.Contains(stderr, "administration") {
		t.Errorf("sans droits : %d %q", code, stderr)
	}
}

func TestRestoreOutsideSources(t *testing.T) {
	_, d, _ := restoreFixture()
	if code, _, stderr := restoreRun(d, `F:\Ailleurs\x.txt`, "--versions"); code != exitRestoreConfig || !strings.Contains(stderr, "hors des sources") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestRestoreVersions(t *testing.T) {
	l, d, _ := restoreFixture()
	code, out, stderr := restoreRun(d, `C:\Users\alice\Documents\Factures\2026.pdf`, "--versions")
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	if !strings.Contains(out, "version actuelle") || !strings.Contains(out, "2 version(s)") || !strings.Contains(out, "1,5 Ko") {
		t.Errorf("%s", out)
	}
	if l.prefixes[0] != "acme-portable01/documents/Factures/2026.pdf" {
		t.Errorf("préfixe %v", l.prefixes)
	}

	// casse différente : relecture du job entier, comparaison sans casse
	l.prefixes = nil
	code, out, _ = restoreRun(d, `c:\users\alice\documents\NOTES.TXT`, "--versions", "--json")
	if code != 0 || len(l.prefixes) != 2 || l.prefixes[1] != "acme-portable01/documents/" {
		t.Fatalf("%d %v", code, l.prefixes)
	}
	var j struct {
		Versions []struct {
			Deleted bool `json:"deleted"`
			Current bool `json:"current"`
		} `json:"versions"`
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil || len(j.Versions) != 2 || !j.Versions[0].Deleted || !j.Versions[0].Current {
		t.Fatalf("%v %s", err, out)
	}

	// dossier ou source entière : --list attendu
	if code, _, stderr := restoreRun(d, `C:\Users\alice\Documents\Factures`, "--versions"); code != exitRestoreRefused || !strings.Contains(stderr, "dossier") {
		t.Errorf("dossier : %d %q", code, stderr)
	}
	if code, _, stderr := restoreRun(d, `C:\Users\alice\Documents`, "--versions"); code != exitRestoreRefused || !strings.Contains(stderr, "source entière") {
		t.Errorf("racine : %d %q", code, stderr)
	}
	// jamais sauvegardé
	if code, out, _ := restoreRun(d, `C:\Users\alice\Documents\inconnu.txt`, "--versions"); code != 0 || !strings.Contains(out, "Aucune version") {
		t.Errorf("inconnu : %d %q", code, out)
	}
}

func TestRestoreList(t *testing.T) {
	l, d, _ := restoreFixture()
	code, out, stderr := restoreRun(d, "--list", `C:\Users\alice\Documents`)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	// dernière sauvegarde : Notes.txt supprimé n'y est pas
	if !strings.Contains(out, "Factures/2026.pdf") || strings.Contains(out, "Notes.txt") || !strings.Contains(out, "1 fichier(s)") {
		t.Errorf("%s", out)
	}
	if l.prefixes[0] != "acme-portable01/documents" {
		t.Errorf("préfixe %v", l.prefixes)
	}

	// à une date : ancienne version, fichier supprimé depuis
	code, out, _ = restoreRun(d, `C:\Users\alice\Documents`, "--list", "--at", restoreNow.AddDate(0, 0, -10).Format("2006-01-02"), "--json")
	if code != 0 {
		t.Fatal(code)
	}
	var j struct {
		At      *time.Time `json:"at"`
		Targets []struct {
			Job   string `json:"job"`
			Total int64  `json:"total_size"`
			Files []struct {
				Path         string     `json:"path"`
				VersionID    string     `json:"version_id"`
				DeletedSince *time.Time `json:"deleted_since"`
				NewerSince   bool       `json:"newer_since"`
			} `json:"files"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		t.Fatal(err)
	}
	f := j.Targets[0].Files
	if j.At == nil || len(f) != 2 || f[0].VersionID != "f1" || !f[0].NewerSince || f[1].Path != "Notes.txt" || f[1].DeletedSince == nil || j.Targets[0].Total != 1010 {
		t.Fatalf("%s", out)
	}

	// --all : un état par job
	code, out, _ = restoreRun(d, "--all", "--list")
	if code != 0 || !strings.Contains(out, "(job documents)") || !strings.Contains(out, "(job projets)") || !strings.Contains(out, "2,0 Ko") {
		t.Fatalf("%d %s", code, out)
	}

	// au-delà de 90 jours : avertissement
	_, _, stderr = restoreRun(d, `C:\Users\alice\Documents`, "--list", "--at", "2026-06-01")
	if !strings.Contains(stderr, "90 jours") {
		t.Errorf("avertissement absent : %q", stderr)
	}
	// date à venir : état de la dernière sauvegarde, sans avertissement
	code, out, stderr = restoreRun(d, `C:\Users\alice\Documents`, "--list", "--at", "2027-01-01")
	if code != 0 || stderr != "" || !strings.Contains(out, "à la dernière sauvegarde") {
		t.Errorf("%d %q %q", code, out, stderr)
	}
}

func TestRestoreBucketError(t *testing.T) {
	l, d, _ := restoreFixture()
	l.err = errors.New("AccessDenied")
	if code, _, stderr := restoreRun(d, `D:\Projets`, "--list"); code != exitRestoreBucket || !strings.Contains(stderr, "AccessDenied") {
		t.Fatalf("%d %q", code, stderr)
	}
	d.bucket = func(*config.Config) (restore.Lister, error) { return nil, errors.New("profile.aws_credentials absent") }
	if code, _, _ := restoreRun(d, `D:\Projets`, "--list"); code != exitRestoreConfig {
		t.Fatalf("clé absente : %d", code)
	}
}

func TestParseRestoreDate(t *testing.T) {
	loc := time.FixedZone("Paris", 3600)
	got, err := parseRestoreDate("2026-10-20", loc)
	if err != nil || !got.Equal(time.Date(2026, 10, 20, 23, 59, 59, 0, loc)) {
		t.Fatalf("%v %v", got, err)
	}
	got, err = parseRestoreDate("2026-10-20 18:00", loc)
	if err != nil || !got.Equal(time.Date(2026, 10, 20, 18, 0, 0, 0, loc)) {
		t.Fatalf("%v %v", got, err)
	}
	for _, s := range []string{"20/10/2026", "2026-13-01", "", "demain"} {
		if _, err := parseRestoreDate(s, loc); err == nil {
			t.Errorf("%q accepté", s)
		}
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 o", 1023: "1023 o", 1536: "1,5 Ko", 5 << 20: "5,0 Mo", 3 << 40: "3,0 To"} {
		if got := humanSize(n); got != want {
			t.Errorf("%d : %q, attendu %q", n, got, want)
		}
	}
}

func TestSplitPositional(t *testing.T) {
	pos, flags := splitPositional([]string{"--at", "2026-10-01", `C:\a b`, "--list", "--config=x.yaml"}, map[string]bool{"at": true})
	if len(pos) != 1 || pos[0] != `C:\a b` || strings.Join(flags, "|") != "--at|2026-10-01|--list|--config=x.yaml" {
		t.Fatalf("%q %q", pos, flags)
	}
	pos, _ = splitPositional([]string{"--list", "--", "-étrange"}, nil)
	if len(pos) != 1 || pos[0] != "-étrange" {
		t.Fatalf("%q", pos)
	}
}
