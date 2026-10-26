package main

import (
	"testing"
	"time"
)

func TestSuspendUntil(t *testing.T) {
	now := time.Date(2026, 10, 26, 21, 0, 0, 0, time.Local)
	if u, on, err := suspendUntil("1h", now); err != nil || !on || !u.Equal(now.Add(time.Hour)) {
		t.Fatalf("1h : %v %v %v", u, on, err)
	}
	if u, on, _ := suspendUntil("tomorrow", now); !on || u.Format("2006-01-02 15:04") != "2026-10-27 08:00" {
		t.Fatalf("tomorrow : %v", u)
	}
	if _, on, err := suspendUntil("off", now); on || err != nil {
		t.Fatal("off")
	}
	for _, bad := range []string{"-1h", "0s", "lundi", "800h"} {
		if _, _, err := suspendUntil(bad, now); err == nil {
			t.Errorf("%q accepté", bad)
		}
	}
}
