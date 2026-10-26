package setup

import (
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		p, err := Password(40)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 40 || seen[p] {
			t.Fatalf("mot de passe %q (longueur %d, déjà vu %v)", p, len(p), seen[p])
		}
		seen[p] = true
		for _, set := range []string{"ABCDEFGHJKLMNPQRSTUVWXYZ", "abcdefghijkmnopqrstuvwxyz", "23456789", "!#%+-.=?@_~"} {
			if !strings.ContainsAny(p, set) {
				t.Fatalf("%q sans caractère de %q", p, set)
			}
		}
	}
	if _, err := Password(3); err == nil {
		t.Fatal("mot de passe de 3 caractères accepté")
	}
}
