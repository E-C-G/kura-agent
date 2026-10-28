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
// qui le reçoit, en privé : 0600 (0700 pour les dossiers créés), le bucket ne gardant pas
// les droits d'origine.
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
	for p, mode := range map[string]os.FileMode{"alice/Documents": 0o700, "alice/Documents/a.txt": 0o600} {
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

// Un fichier remplacé garde le mode et le propriétaire de l'actuel.
func TestReplaceKeepsRights(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root nécessaire")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "shadow")
	os.WriteFile(p, []byte("actuel"), 0o640)
	os.Chown(p, 0, 42)
	d, err := OpenDest(dir, Owner{Set: true, Inherit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := &memBucket{data: map[string][]byte{"v1": []byte("ancienne")}}
	if _, err := d.Replace(context.Background(), m, "shadow", Version{Key: "k", VersionID: "v1", Size: 8}, "x"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	st := fi.Sys().(*syscall.Stat_t)
	if fi.Mode().Perm() != 0o640 || st.Uid != 0 || st.Gid != 42 {
		t.Fatalf("%v %d:%d", fi.Mode().Perm(), st.Uid, st.Gid)
	}
}
