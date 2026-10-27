package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallBinaryFrom(t *testing.T) {
	// Déjà dans /usr/bin (paquet) : gardé tel quel.
	if exe, gui, err := installBinaryFrom("/usr/bin/kura-agent", "/usr/local/bin"); err != nil || exe != "/usr/bin/kura-agent" || gui != "" {
		t.Fatalf("%s %s %v", exe, gui, err)
	}
	// Ailleurs (téléchargement) : copié, exécutable.
	dir := t.TempDir()
	src := filepath.Join(dir, "dl", "kura-agent")
	os.MkdirAll(filepath.Dir(src), 0o755)
	os.WriteFile(src, []byte("ELF"), 0o644)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	exe, _, err := installBinaryFrom(src, bin)
	if err != nil || exe != filepath.Join(bin, "kura-agent") {
		t.Fatalf("%s %v", exe, err)
	}
	if st, err := os.Stat(exe); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("%v %v", st, err)
	}
	// Relancé depuis la copie : rien à copier.
	if again, _, err := installBinaryFrom(exe, bin); err != nil || again != exe {
		t.Fatalf("%s %v", again, err)
	}
}
