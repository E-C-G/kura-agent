package tray

import (
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/ipc"
)

func TestRestoreRows(t *testing.T) {
	d := time.Date(2026, 10, 20, 22, 30, 0, 0, time.Local)
	del := time.Date(2026, 10, 25, 9, 0, 0, 0, time.Local)
	r := FileRow(ipc.RestoreFile{Rel: "Factures/2026.pdf", Size: 1536, BackedUp: d, DeletedSince: &del})
	if r != [4]string{`Factures\2026.pdf`, "1,5 Ko", "20/10/2026 22:30", "supprimé le 25/10/2026"} {
		t.Fatalf("%q", r)
	}
	if r := FileRow(ipc.RestoreFile{Rel: "a", Newer: true, BackedUp: d}); r[3] != "modifié depuis" {
		t.Fatalf("%q", r)
	}
	if r := FileRow(ipc.RestoreFile{Rel: "../x", Problem: "segment refusé", BackedUp: d}); !strings.HasPrefix(r[3], "non restaurable") {
		t.Fatalf("%q", r)
	}
	if v := VersionRow(ipc.RestoreVersion{Date: d, Deleted: true}); v[1] != "—" || v[2] != "supprimé du poste" {
		t.Fatalf("%q", v)
	}
	if v := VersionRow(ipc.RestoreVersion{Date: d, Size: 10, Current: true}); v[2] != "version actuelle" {
		t.Fatalf("%q", v)
	}
}

func TestStateAtCalendar(t *testing.T) {
	now := time.Date(2026, 10, 28, 14, 0, 0, 0, time.Local)
	if StateAt(now, now) != nil || StateAt(now.AddDate(0, 0, 1), now) != nil {
		t.Fatal("aujourd'hui et plus tard : dernière sauvegarde")
	}
	at := StateAt(time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local), now)
	if at == nil || !at.Equal(time.Date(2026, 10, 20, 23, 59, 59, 0, time.Local)) {
		t.Fatalf("%v", at)
	}
}

func TestRestoreTexts(t *testing.T) {
	if s := ListSummary(0, 0, nil); !strings.Contains(s, "Aucun fichier") {
		t.Fatal(s)
	}
	at := time.Date(2026, 10, 20, 23, 59, 59, 0, time.Local)
	if s := ListSummary(37, 441344, &at); !strings.Contains(s, "37 fichier(s), 431,0 Ko, au 20/10/2026") {
		t.Fatal(s)
	}
	if s := ConfirmText(2, 2048, `D:\Projets`, ipc.RestoreMissing); !strings.Contains(s, "qui n'y sont plus") {
		t.Fatal(s)
	}
	if s := ConfirmText(2, 2048, `D:\Projets`, ipc.RestoreCopy); !strings.Contains(s, "nouveau dossier") || !strings.Contains(s, "Aucun fichier existant") {
		t.Fatal(s)
	}
	title, text := DoneText(true, ipc.RestoreData{Done: 3, Bytes: 2048, Kept: 1}, "")
	if title != "Restauration terminée" || text != "3 fichier(s) restauré(s), 2,0 Ko, 1 déjà à leur place." {
		t.Fatalf("%q %q", title, text)
	}
	title, text = DoneText(false, ipc.RestoreData{}, "Une sauvegarde est en cours.")
	if title != "Restauration non faite" || text != "Une sauvegarde est en cours." {
		t.Fatalf("%q %q", title, text)
	}
	if title, _ := DoneText(false, ipc.RestoreData{Done: 1, Failed: 2}, ""); title != "Restauration incomplète" {
		t.Fatal(title)
	}
	if n := PickSize([]ipc.RestoreFile{{Size: 1}, {Size: 2}, {Size: 4}}, []int{0, 2, 9}); n != 5 {
		t.Fatal(n)
	}
}
