package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestMain : la copie du binaire de test lancée par TestSwapRunningExecutable dort au
// lieu de lancer les tests.
func TestMain(m *testing.M) {
	if os.Getenv("KURA_SELFUPDATE_SLEEP") == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Un exécutable en cours d'exécution (la commande elle-même, ou l'icône sous Windows)
// se remplace et se remet en place.
func TestSwapRunningExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "kura-agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	if err := copyFile(self, target); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(target)
	cmd.Env = append(os.Environ(), "KURA_SELFUPDATE_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	time.Sleep(300 * time.Millisecond)

	newFile := filepath.Join(t.TempDir(), "nouveau")
	if err := os.WriteFile(newFile, []byte("nouvelle version"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Swap([]File{{Target: target, New: newFile}}); err != nil {
		t.Fatalf("remplacement d'un exécutable en cours d'exécution : %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "nouvelle version" {
		t.Fatal("cible non remplacée")
	}
	// Retour arrière pendant que l'ancienne version tourne toujours.
	if err := Restore([]string{target}); err != nil {
		t.Fatalf("retour arrière : %v", err)
	}
	if st, err := os.Stat(target); err != nil || st.Size() < 1000 {
		t.Fatal("ancienne version non remise")
	}
	// Seconde mise à jour alors que la sauvegarde précédente tourne encore (icône
	// lancée depuis l'ancienne version) : elle est écartée, pas bloquante.
	if err := Swap([]File{{Target: target, New: newFile}}); err != nil {
		t.Fatal(err)
	}
	if err := Swap([]File{{Target: target, New: newFile}}); err != nil {
		t.Fatalf("sauvegarde encore utilisée : %v", err)
	}
}
