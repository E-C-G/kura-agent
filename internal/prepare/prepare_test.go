package prepare

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// fakeZip joue Info-ZIP : lit les noms sur l'entrée standard et écrit une vraie archive.
type fakeZip struct {
	code  int    // code de sortie
	extra string // entrée ajoutée sans avoir été demandée
	drop  string // fichier demandé mais laissé de côté (verrouillé)
	specs []execx.Spec
}

func (f *fakeZip) RunSpec(ctx context.Context, s execx.Spec) (int, error) {
	f.specs = append(f.specs, s)
	archive := s.Args[len(s.Args)-2]
	out, err := os.Create(archive)
	if err != nil {
		return -1, err
	}
	defer out.Close()
	w := zip.NewWriter(out)
	sc := bufio.NewScanner(s.Stdin)
	for sc.Scan() {
		name := sc.Text()
		if name == f.drop {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(name)))
		if err != nil {
			return -1, err
		}
		e, _ := w.Create(name)
		e.Write(data)
	}
	if f.extra != "" {
		e, _ := w.Create(f.extra)
		e.Write([]byte("intrus"))
	}
	return f.code, w.Close()
}

func okTool(context.Context, Runner, string) (string, error) { return "3.0", nil }

type fixture struct {
	cfg     *config.Config
	staging string
	src     string
}

func newFixture(t *testing.T, tool string) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{staging: filepath.Join(dir, "staging"), src: filepath.Join(dir, "Courrier")}
	for name, data := range map[string]string{"a.txt": "a", "sous/b.txt": "bb", "été.txt": "é", "x.tmp": "tmp"} {
		p := filepath.Join(f.src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(f.staging, 0o700); err != nil {
		t.Fatal(err)
	}
	f.cfg = &config.Config{
		Tools:   config.Tools{Zip: tool},
		Prepare: []config.Prepare{{ID: "courrier", Type: config.PrepareZip, Source: f.src, Exclude: []string{"*.tmp"}}},
	}
	f.cfg.ApplyDefaults()
	return f
}

func (f fixture) run(t *testing.T, r Runner, check func(context.Context, Runner, string) (string, error)) state.PrepareStatus {
	t.Helper()
	return Run(context.Background(), f.cfg, f.cfg.Prepare[0], Deps{Exec: r, StagingDir: f.staging, CheckTool: check})
}

func entries(t *testing.T, archive string) []string {
	t.Helper()
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	return names
}

func TestZipWithFakeTool(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "zip"))
	tool := &fakeZip{}
	ps := f.run(t, tool, okTool)
	if ps.Result != state.ResultOK || ps.Files != 3 || ps.Size == 0 {
		t.Fatalf("préparation : %+v", ps)
	}
	got := entries(t, filepath.Join(f.staging, "courrier", "courrier.zip"))
	if want := []string{"a.txt", "sous/b.txt", "été.txt"}; !slices.Equal(got, want) {
		t.Fatalf("entrées %v, attendu %v", got, want)
	}
	s := tool.specs[0]
	// L'outil travaille sur la copie de la zone de préparation, jamais dans la source.
	if s.Dir != filepath.Join(f.staging, "courrier.src") || !slices.Contains(s.Args, "-y") || !slices.Contains(s.Args, "-nw") {
		t.Errorf("lancement : %+v", s)
	}
	for _, e := range s.Env {
		if strings.HasPrefix(e, "AWS_") || strings.HasPrefix(e, "ZIPOPT") {
			t.Errorf("variable de l'agent passée à l'outil : %s", e)
		}
	}
	for _, left := range []string{"courrier.tmp", "courrier.src", "courrier.list"} {
		if _, err := os.Stat(filepath.Join(f.staging, left)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s resté", left)
		}
	}
}

func TestZipFailures(t *testing.T) {
	for _, c := range []struct {
		name     string
		tool     fakeZip
		onLocked string
		result   string
		msg      string
	}{
		{"code d'erreur", fakeZip{code: 2}, config.OnLockedFail, state.ResultError, "code 2"},
		{"entrée en trop", fakeZip{extra: "../coffre.secret"}, config.OnLockedFail, state.ResultError, "non demandée"},
		{"fichier manquant", fakeZip{drop: "a.txt"}, config.OnLockedFail, state.ResultError, "absent"},
		{"verrouillé, fail", fakeZip{drop: "a.txt", code: 18}, config.OnLockedFail, state.ResultError, "code 18"},
		{"verrouillé, skip", fakeZip{drop: "a.txt", code: 18}, config.OnLockedSkip, state.ResultWarning, "a.txt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, filepath.Join(t.TempDir(), "zip"))
			f.cfg.Prepare[0].OnLocked = c.onLocked
			tool := c.tool
			ps := f.run(t, &tool, okTool)
			if ps.Result != c.result || !strings.Contains(ps.Message, c.msg) {
				t.Fatalf("%+v", ps)
			}
			_, err := os.Stat(filepath.Join(f.staging, "courrier"))
			if exists := err == nil; exists != (c.result == state.ResultWarning) {
				t.Errorf("résultat présent : %v", exists)
			}
			left, _ := os.ReadDir(f.staging)
			if c.result == state.ResultError && len(left) != 0 {
				t.Errorf("zone non nettoyée : %v", left)
			}
		})
	}
}

func TestZipRefusesBadToolAndEmptySource(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "zip"))
	refuse := func(context.Context, Runner, string) (string, error) { return "", errors.New("modifiable par alice") }
	if ps := f.run(t, &fakeZip{}, refuse); ps.Result != state.ResultError || !strings.Contains(ps.Message, "tools.zip") {
		t.Fatalf("outil refusé accepté : %+v", ps)
	}
	f.cfg.Prepare[0].Exclude = []string{"*"}
	if ps := f.run(t, &fakeZip{}, okTool); ps.Result != state.ResultError || !strings.Contains(ps.Message, "rien à archiver") {
		t.Fatalf("source vide acceptée : %+v", ps)
	}
	f.cfg.Prepare[0].Unavailable = "prepare[0].source : profil introuvable"
	if ps := f.run(t, &fakeZip{}, okTool); ps.Result != state.ResultError || !strings.Contains(ps.Message, "profil") {
		t.Fatalf("source indisponible acceptée : %+v", ps)
	}
}

func TestVerifyZipDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.zip")
	out, _ := os.Create(p)
	w := zip.NewWriter(out)
	e, _ := w.CreateHeader(&zip.FileHeader{Name: "a.txt", Method: zip.Store})
	e.Write([]byte("contenu intact"))
	w.Close()
	out.Close()
	if names, _, err := verifyZip(p); err != nil || len(names) != 1 {
		t.Fatalf("%v %v", names, err)
	}
	data, _ := os.ReadFile(p)
	i := strings.Index(string(data), "intact")
	data[i] = 'X'
	os.WriteFile(p, data, 0o644)
	if _, _, err := verifyZip(p); err == nil {
		t.Fatal("archive altérée acceptée")
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700)
	os.WriteFile(filepath.Join(dir, "x.list"), nil, 0o600)
	if err := Clear(dir); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("restes : %v", left)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("la zone elle-même doit rester")
	}
	if err := Clear(filepath.Join(dir, "absent")); err != nil {
		t.Fatal(err)
	}
}

// realZip : l'outil zip du système de test (Info-ZIP sous Linux et macOS, 7-Zip sous
// Windows), ou rien.
func realZip(t *testing.T) string {
	t.Helper()
	candidates := []string{"/usr/bin/zip"}
	if runtime.GOOS == "windows" {
		candidates = []string{`C:\Program Files\7-Zip\7z.exe`}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("outil zip absent de la machine de CI : %v", candidates)
	}
	t.Skip("pas d'outil zip sur ce poste")
	return ""
}

// Le vrai outil, vérifié par CheckZip (droits, version) : les liens ne sont pas suivis,
// les dossiers de l'agent ni les exclusions n'entrent dans l'archive.
func TestZipWithRealTool(t *testing.T) {
	tool := realZip(t)
	f := newFixture(t, tool)
	outside := filepath.Join(filepath.Dir(f.src), "dehors")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)
	link := filepath.Join(f.src, "lien")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Fatalf("mklink : %v %s", err, out)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(f.src, "Kura")
	os.MkdirAll(agent, 0o700)
	os.WriteFile(filepath.Join(agent, "coffre.secret"), []byte("k"), 0o600)

	var log strings.Builder
	for _, level := range []string{config.LevelDeflate, config.LevelStore} {
		f.cfg.Prepare[0].Level = level
		ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], Deps{
			Exec: execx.System{}, StagingDir: f.staging, AgentDirs: []string{agent}, Out: &log,
		})
		if ps.Result != state.ResultOK || ps.Files != 3 {
			t.Fatalf("%s : %+v\n%s", level, ps, log.String())
		}
		got := entries(t, filepath.Join(f.staging, "courrier", "courrier.zip"))
		if want := []string{"a.txt", "sous/b.txt", "été.txt"}; !slices.Equal(got, want) {
			t.Fatalf("%s : entrées %v, attendu %v\n%s", level, got, want, log.String())
		}
	}
}

func TestCheckToolRefusesUserWritable(t *testing.T) {
	tool := filepath.Join(t.TempDir(), "zip")
	if runtime.GOOS == "windows" {
		tool += ".exe"
	}
	if err := os.WriteFile(tool, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("tests lancés par root : le dossier temporaire est à root")
	}
	if err := CheckTool(tool); err == nil {
		t.Fatal("outil dans un dossier de l'utilisateur accepté")
	}
	if err := CheckTool("zip"); err == nil {
		t.Fatal("chemin relatif accepté")
	}
	if err := CheckTool(filepath.Join(t.TempDir(), "absent")); err == nil || !strings.Contains(err.Error(), "introuvable") {
		t.Fatalf("outil absent : %v", err)
	}
}

func TestCheckZipVersion(t *testing.T) {
	tool := realZip(t)
	v, err := CheckZip(context.Background(), execx.System{}, tool)
	if err != nil || v == "" {
		t.Fatalf("%s : %q %v", tool, v, err)
	}
	old := versionRunner{"7-Zip [64] 9.20 : Copyright (c) 1999-2010 Igor Pavlov"}
	if m := sevenZipVersion.FindStringSubmatch(old.out); m == nil || m[1] != "9" || m[2] != "20" {
		t.Fatalf("version 7-Zip mal lue : %v", m)
	}
	if m := sevenZipVersion.FindStringSubmatch("7-Zip (z) 23.01 (x64) : Copyright"); m == nil || m[1] != "23" {
		t.Fatalf("version 7-Zip mal lue : %v", m)
	}
}

type versionRunner struct{ out string }

func (v versionRunner) RunSpec(_ context.Context, s execx.Spec) (int, error) {
	io.WriteString(s.Out, v.out)
	return 0, nil
}

// Une source qui est (ou se trouve sous) un dossier de l'agent est refusée, quel que soit
// le chemin employé pour le désigner.
func TestZipRefusesAgentDirSource(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "zip"))
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], Deps{
		Exec: &fakeZip{}, StagingDir: f.staging, CheckTool: okTool, AgentDirs: []string{filepath.Dir(f.src)},
	})
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "dossier de l'agent") {
		t.Fatalf("%+v", ps)
	}
}

// Un dossier remplacé par un lien vers l'extérieur entre le parcours et la copie : le
// fichier n'est jamais lu hors de la source.
func TestCopyOneNeverLeavesSource(t *testing.T) {
	dir := t.TempDir()
	src, outside, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dehors"), filepath.Join(dir, "copie")
	os.MkdirAll(filepath.Join(src, "a"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "f.txt"), []byte("ordinaire"), 0o644)
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "f.txt"), []byte("SECRET"), 0o644)
	root, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// Le parcours a vu a/f.txt ; « a » devient un lien vers l'extérieur avant la copie.
	os.RemoveAll(filepath.Join(src, "a"))
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(src, "a"), outside).CombinedOutput(); err != nil {
			t.Fatalf("mklink : %v %s", err, out)
		}
	} else if err := os.Symlink(outside, filepath.Join(src, "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := copyOne(context.Background(), root, "a/f.txt", dst); err == nil {
		data, _ := os.ReadFile(filepath.Join(dst, "a", "f.txt"))
		t.Fatalf("fichier lu à travers le lien : %q", data)
	}
}
