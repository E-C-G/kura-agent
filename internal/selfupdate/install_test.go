package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func content(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSwapAndRestore(t *testing.T) {
	dir := t.TempDir()
	exe, gui := filepath.Join(dir, "kura-agent.exe"), filepath.Join(dir, "kura-agentw.exe")
	write(t, exe, "ancien")
	write(t, gui, "ancien-w")
	newDir := t.TempDir()
	write(t, filepath.Join(newDir, "a"), "nouveau")
	write(t, filepath.Join(newDir, "b"), "nouveau-w")
	write(t, exe+BackupSuffix, "très ancien") // sauvegarde d'une mise à jour précédente

	if err := Swap([]File{{exe, filepath.Join(newDir, "a")}, {gui, filepath.Join(newDir, "b")}}); err != nil {
		t.Fatal(err)
	}
	if content(t, exe) != "nouveau" || content(t, gui) != "nouveau-w" || content(t, exe+BackupSuffix) != "ancien" || content(t, gui+BackupSuffix) != "ancien-w" {
		t.Fatal("remplacement")
	}
	if err := Restore([]string{exe, gui}); err != nil {
		t.Fatal(err)
	}
	if content(t, exe) != "ancien" || content(t, gui) != "ancien-w" {
		t.Fatal("retour arrière")
	}
	if _, err := os.Stat(exe + ".failed"); err == nil {
		t.Error("version défaite restée")
	}
	// Plus de sauvegarde : un second retour arrière échoue sans rien toucher.
	if err := Restore([]string{exe}); err == nil || content(t, exe) != "ancien" {
		t.Fatalf("second retour arrière : %v", err)
	}
}

// Le second fichier ne peut pas être remplacé : le premier est remis.
func TestSwapPartialFailureRestores(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "kura-agent.exe")
	write(t, exe, "ancien")
	newDir := t.TempDir()
	write(t, filepath.Join(newDir, "a"), "nouveau")
	err := Swap([]File{{exe, filepath.Join(newDir, "a")}, {filepath.Join(dir, "absent.exe"), filepath.Join(newDir, "a")}})
	if err == nil || content(t, exe) != "ancien" {
		t.Fatalf("%v ; contenu %q", err, content(t, exe))
	}
}

func TestCleanBackups(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "kura-agent.exe")
	for _, n := range []string{exe, exe + ".old", exe + ".old-abc", exe + ".failed", exe + ".new"} {
		write(t, n, "x")
	}
	CleanBackups([]string{exe})
	for n, want := range map[string]bool{exe: true, exe + ".old": true, exe + ".old-abc": false, exe + ".failed": false, exe + ".new": false} {
		if _, err := os.Stat(n); (err == nil) != want {
			t.Errorf("%s : présent=%v", filepath.Base(n), err == nil)
		}
	}
}

func makeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	zw.Close()
	write(t, path, b.String())
}

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "a.zip")
	makeZip(t, arch, map[string]string{
		"kura-agent_1.2.3_windows_amd64/kura-agent.exe":  "exe",
		"kura-agent_1.2.3_windows_amd64/kura-agentw.exe": "gui",
		"kura-agent_1.2.3_windows_amd64/README.md":       "lisez-moi",
		"../kura-agent.exe":                              "évasion",
		"autre/kura-agent.exe":                           "autre dossier",
	})
	out := filepath.Join(dir, "out")
	os.Mkdir(out, 0o700)
	if err := ExtractZip(arch, "kura-agent_1.2.3_windows_amd64", out, "kura-agent.exe", "kura-agentw.exe"); err != nil {
		t.Fatal(err)
	}
	if content(t, filepath.Join(out, "kura-agent.exe")) != "exe" || content(t, filepath.Join(out, "kura-agentw.exe")) != "gui" {
		t.Fatal("contenu extrait")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 2 {
		t.Fatalf("fichiers extraits : %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "kura-agent.exe")); err == nil {
		t.Fatal("chemin de l'archive suivi hors du dossier")
	}
	out2 := filepath.Join(dir, "out2")
	os.Mkdir(out2, 0o700)
	if err := ExtractZip(arch, "kura-agent_9.9.9_windows_amd64", out2, "kura-agent.exe"); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("mauvais dossier : %v", err)
	}
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	add := func(name, body string, typ byte) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: typ, Linkname: "/etc/passwd"})
		tw.Write([]byte(body))
	}
	add("kura-agent_1.2.3_darwin_arm64/kura-agent", "mac", tar.TypeReg)
	add("kura-agent_1.2.3_darwin_arm64/lien", "", tar.TypeSymlink)
	tw.Close()
	gz.Close()
	arch := filepath.Join(dir, "a.tar.gz")
	write(t, arch, b.String())
	out := filepath.Join(dir, "out")
	os.Mkdir(out, 0o700)
	if err := ExtractTarGz(arch, "kura-agent_1.2.3_darwin_arm64", out, "kura-agent"); err != nil {
		t.Fatal(err)
	}
	if content(t, filepath.Join(out, "kura-agent")) != "mac" {
		t.Fatal("contenu")
	}
	out2 := filepath.Join(dir, "out2")
	os.Mkdir(out2, 0o700)
	if err := ExtractTarGz(arch, "kura-agent_1.2.3_darwin_arm64", out2, "lien"); err == nil {
		t.Fatal("lien symbolique extrait")
	}
}

func TestDownload(t *testing.T) {
	body := []byte(strings.Repeat("archive ", 1000))
	sum := sha256.Sum256(body)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/court" {
			w.Write(body[:10])
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	dir := t.TempDir()
	a := Asset{Name: "a.zip", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), URL: srv.URL + "/a.zip"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Download(ctx, a, filepath.Join(dir, "ok")); err != nil {
		t.Fatal(err)
	}
	bad := a
	bad.SHA256 = strings.Repeat("0", 64)
	if err := c.Download(ctx, bad, filepath.Join(dir, "bad")); err == nil || !strings.Contains(err.Error(), "empreinte") {
		t.Fatalf("empreinte : %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad")); err == nil {
		t.Error("archive refusée gardée")
	}
	short := a
	short.URL = srv.URL + "/court"
	if err := c.Download(ctx, short, filepath.Join(dir, "short")); err == nil {
		t.Error("archive incomplète acceptée")
	}
	plain := a
	plain.URL = strings.Replace(srv.URL, "https://", "http://", 1) + "/a.zip"
	if err := c.Download(ctx, plain, filepath.Join(dir, "plain")); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Errorf("HTTP accepté : %v", err)
	}
	big := a
	big.Size = MaxAssetSize + 1
	if err := c.Download(ctx, big, filepath.Join(dir, "big")); err == nil {
		t.Error("archive trop grande acceptée")
	}
}

func TestStatusCheckDue(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	recent, old, future := now.Add(-time.Hour), now.Add(-25*time.Hour), now.Add(time.Hour)
	for _, c := range []struct {
		at   *time.Time
		want bool
	}{{nil, true}, {&recent, false}, {&old, true}, {&future, true}} {
		if got := (Status{CheckedAt: c.at}).CheckDue(now); got != c.want {
			t.Errorf("%v : %v", c.at, got)
		}
	}
}
