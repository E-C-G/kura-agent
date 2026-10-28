//go:build !windows

package restore

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// En place, un fichier restauré par root prend le propriétaire et le groupe du dossier
// qui le reçoit, et les droits 0644 (0755 pour les dossiers créés).
func TestInheritOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root nécessaire")
	}
	dir := t.TempDir()
	alice := filepath.Join(dir, "alice")
	os.Mkdir(alice, 0o700)
	os.Chown(alice, 1234, 2345)
	d, err := OpenDest(dir, Owner{Set: true, Inherit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := &memBucket{data: map[string][]byte{"v1": []byte("bonjour")}}
	if err := d.Write(context.Background(), m, "alice/Documents/a.txt", Version{Key: "k", VersionID: "v1", Size: 7}); err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]os.FileMode{"alice/Documents": 0o755, "alice/Documents/a.txt": 0o644} {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if st.Uid != 1234 || st.Gid != 2345 || fi.Mode().Perm() != mode {
			t.Errorf("%s : %d:%d %v", p, st.Uid, st.Gid, fi.Mode().Perm())
		}
	}
}
