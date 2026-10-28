//go:build windows

package prepare

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
	"golang.org/x/sys/windows"
)

// 7-Zip, installé dans Program Files, passe le contrôle des droits.
func TestCheckToolAcceptsProgramFiles(t *testing.T) {
	tool := realZip(t)
	if err := CheckTool(tool); err != nil {
		t.Fatal(err)
	}
}

// Un fichier ouvert sans partage : refusé avec on_locked: fail, laissé de côté avec skip.
func TestZipLockedFile(t *testing.T) {
	tool := realZip(t)
	f := newFixture(t, tool)
	locked := filepath.Join(f.src, "a.txt")
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(locked), windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var log strings.Builder
	d := Deps{Exec: execx.System{}, StagingDir: f.staging, Out: &log}
	if ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], d); ps.Result != state.ResultError {
		t.Fatalf("fichier verrouillé accepté avec fail : %+v\n%s", ps, log.String())
	}
	f.cfg.Prepare[0].OnLocked = config.OnLockedSkip
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[0], d)
	if ps.Result != state.ResultWarning || ps.Missing != 1 || ps.Files != 2 {
		t.Fatalf("skip : %+v\n%s", ps, log.String())
	}
}

// Une jonction à la place de la zone de préparation est refusée.
func TestEnsureStagingRefusesJunction(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureStaging(filepath.Join(dir, "staging")); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "ailleurs")
	os.Mkdir(other, 0o755)
	link := filepath.Join(dir, "lien")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, other).CombinedOutput(); err != nil {
		t.Fatalf("mklink : %v %s", err, out)
	}
	if err := EnsureStaging(link); err == nil {
		t.Fatal("jonction acceptée comme zone de préparation")
	}
}
