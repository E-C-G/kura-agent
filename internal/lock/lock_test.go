package lock

import (
	"errors"
	"testing"
)

func TestSecondAcquireIsBusy(t *testing.T) {
	release, err := acquire(`Global\Kura-Test`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(`Global\Kura-Test`); !errors.Is(err, ErrBusy) {
		t.Fatalf("ErrBusy attendu, reçu %v", err)
	}
	release()
	again, err := acquire(`Global\Kura-Test`)
	if err != nil {
		t.Fatalf("après libération, le verrou doit se reprendre : %v", err)
	}
	again()
}
