package tray

import "testing"

func TestPixels(t *testing.T) {
	seen := map[string]Icon{}
	for _, ic := range []Icon{IconUnknown, IconOK, IconRunning, IconWarning, IconError, IconSuspended, IconLate} {
		for _, size := range []int{16, 20, 24, 32} {
			px := Pixels(ic, 0, size)
			if len(px) != size*size*4 {
				t.Fatalf("taille %d", len(px))
			}
			if px[3] != 0 {
				t.Errorf("icône %d : coin supérieur gauche non transparent", ic)
			}
			opaque := 0
			for i := 3; i < len(px); i += 4 {
				if px[i] == 255 {
					opaque++
				}
			}
			if opaque < size*size/4 {
				t.Errorf("icône %d en %d px presque vide (%d pixels opaques)", ic, size, opaque)
			}
		}
		key := string(Pixels(ic, 0, 32))
		if other, dup := seen[key]; dup {
			t.Errorf("icônes %d et %d identiques", ic, other)
		}
		seen[key] = ic
	}
	if string(Pixels(IconRunning, 0, 16)) == string(Pixels(IconRunning, 1, 16)) {
		t.Error("l'icône « en cours » n'est pas animée")
	}
}
