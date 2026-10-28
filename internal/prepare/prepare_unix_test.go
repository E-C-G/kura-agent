//go:build !windows

package prepare

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
)

// Un « \ » est un caractère ordinaire hors Windows ; un tube nommé n'est pas un fichier
// (laissé de côté sans bloquer ni faire échouer l'archive).
func TestZipUnixNames(t *testing.T) {
	tool := realZip(t)
	f := newFixture(t, tool)
	os.WriteFile(filepath.Join(f.src, `b\c.txt`), []byte("x"), 0o644)
	if err := syscall.Mkfifo(filepath.Join(f.src, "tube"), 0o644); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], Deps{Exec: execx.System{}, StagingDir: f.staging, Out: &log})
	if ps.Result != state.ResultOK || ps.Files != 4 {
		t.Fatalf("%+v\n%s", ps, log.String())
	}
	got := entries(t, filepath.Join(f.staging, "courrier", "courrier.zip"))
	if !slices.Contains(got, `b\c.txt`) || slices.Contains(got, "tube") {
		t.Fatalf("entrées : %v", got)
	}
}

// Un fichier illisible est laissé de côté avec on_locked: skip, refusé avec fail.
func TestZipUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lit tout")
	}
	f := newFixture(t, filepath.Join(t.TempDir(), "zip"))
	os.Chmod(filepath.Join(f.src, "a.txt"), 0)
	os.Chmod(filepath.Join(f.src, "sous"), 0)
	defer os.Chmod(filepath.Join(f.src, "sous"), 0o755)
	if ps := f.run(t, &fakeZip{}, okTool); ps.Result != state.ResultError || !strings.Contains(ps.Message, "a.txt") {
		t.Fatalf("fail : %+v", ps)
	}
	f.cfg.Prepare[0].OnLocked = "skip"
	ps := f.run(t, &fakeZip{}, okTool)
	if ps.Result != state.ResultWarning || ps.Missing != 2 || ps.Files != 1 {
		t.Fatalf("skip : %+v", ps)
	}
}
