package tray

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/E-C-G/kura-agent/internal/selfupdate"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

var now = time.Date(2026, 10, 26, 9, 0, 0, 0, time.Local) // lundi

func cfg() *config.Config {
	c := &config.Config{Schedule: config.Schedule{Type: "daily", Time: "22:30", Days: []string{"mon", "tue", "wed", "thu", "fri"}},
		Tray: config.Tray{Users: []string{"alice", "bob"}, AllowRunNow: []string{"alice"}}}
	c.ApplyDefaults()
	return c
}

func tp(t time.Time) *time.Time { return &t }

func okStatus(end time.Time) *state.Status {
	return &state.Status{State: "idle", RunID: end.Format("2006-01-02_1504"), Result: state.ResultOK,
		StartedAt: end.Add(-20 * time.Minute), EndedAt: tp(end), LastSuccess: tp(end)}
}

func TestStates(t *testing.T) {
	yesterday := now.Add(-11 * time.Hour)
	running := okStatus(yesterday)
	running.State, running.Current = "running", &state.Progress{Job: "Données serveur", Index: 1, Total: 2}
	failed := okStatus(yesterday)
	failed.Result, failed.Message = state.ResultGuard, "garde-fou : 812 suppressions prévues"
	failed.LastSuccess = tp(yesterday.Add(-24 * time.Hour))
	warn := okStatus(yesterday)
	warn.Result = state.ResultWarning
	warn.Jobs = []state.JobStatus{{Name: "Outlook alice", Result: state.ResultWarning, Message: "1 fichier(s) ignoré(s)"}}
	old := okStatus(now.Add(-72 * time.Hour))

	for _, c := range []struct {
		name string
		in   Input
		icon Icon
		tip  string
	}{
		{"aucune", Input{}, IconUnknown, "Aucune sauvegarde"},
		{"ok", Input{Status: okStatus(yesterday)}, IconOK, "réussie le 25/10 22:00"},
		{"en cours", Input{Status: running}, IconRunning, "Sauvegarde en cours : Données serveur (1/2)"},
		{"garde-fou", Input{Status: failed}, IconError, "Échec le 25/10 22:00 : garde-fou : 812"},
		{"avertissement", Input{Status: warn}, IconWarning, "Outlook alice – 1 fichier(s) ignoré(s)"},
		{"retard", Input{Status: old}, IconLate, "depuis plus de 2 jours"},
		{"suspendu", Input{Status: old, Suspension: &state.Suspension{Until: now.Add(2 * time.Hour)}}, IconSuspended, "suspendue jusqu'au 26/10 11:00"},
		{"suspension finie", Input{Status: okStatus(yesterday), Suspension: &state.Suspension{Until: now.Add(-time.Hour)}}, IconOK, "réussie"},
		{"config", Input{Status: okStatus(yesterday), ConfigErr: errors.New("configuration invalide (1 erreur(s)) :\n- jobs : vide")}, IconError, "Configuration invalide : configuration invalide (1 erreur(s)) :"},
	} {
		in := c.in
		in.Now = now
		if in.ConfigErr == nil {
			in.Config = cfg()
		}
		v := Compute(in)
		if v.Icon != c.icon || !strings.Contains(v.Tooltip, c.tip) {
			t.Errorf("%s : icône %d, info-bulle %q (attendu %d, %q)", c.name, v.Icon, v.Tooltip, c.icon, c.tip)
		}
		if len([]rune(v.Tooltip)) > 127 {
			t.Errorf("%s : info-bulle trop longue", c.name)
		}
	}
}

func TestMenu(t *testing.T) {
	in := Input{Status: okStatus(now.Add(-time.Hour)), Config: cfg(), User: `SRV01\alice`, Now: now}
	v := Compute(in)
	if !v.ShowRunNow || !v.RunNowEnabled || v.ShowCancel || v.NextLine != "Prochaine : 26/10 22:30" {
		t.Fatalf("alice au repos : %+v", v)
	}
	in.Status.State = "running"
	in.Connected = true
	if v := Compute(in); v.RunNowEnabled || !v.ShowCancel {
		t.Fatalf("alice pendant une exécution : %+v", v)
	}
	in.User = `SRV01\bob`
	if v := Compute(in); v.ShowRunNow || v.ShowCancel {
		t.Fatalf("bob n'a pas le droit de lancer ni d'annuler : %+v", v)
	}
	in.User, in.Status.State = `SRV01\alice`, "idle"
	in.Suspension = &state.Suspension{Until: now.Add(time.Hour)}
	if v := Compute(in); v.RunNowEnabled || v.NextLine != "Suspendue jusqu'au 26/10 10:00" {
		t.Fatalf("suspendu : %+v", v)
	}
}

func TestFinishedToast(t *testing.T) {
	on := []string{"failure", "warning"}
	prev := okStatus(now.Add(-24 * time.Hour))
	running := &state.Status{State: "running", RunID: "r2", StartedAt: now}
	failed := &state.Status{State: "idle", RunID: "r2", Result: state.ResultError, StartedAt: now, EndedAt: tp(now),
		Jobs: []state.JobStatus{{Name: "Données serveur", Result: state.ResultError, Message: "aws : code 255"}}}
	if FinishedToast(nil, failed, on) != nil {
		t.Error("pas de toast au premier chargement")
	}
	if FinishedToast(prev, running, on) != nil {
		t.Error("pas de toast pendant une exécution")
	}
	ts := FinishedToast(running, failed, on)
	if ts == nil || ts.Level != LevelError || !ts.OpenLog || !strings.Contains(ts.Text, "aws : code 255") {
		t.Fatalf("toast d'échec : %+v", ts)
	}
	if FinishedToast(failed, failed, on) != nil {
		t.Error("toast répété pour la même exécution")
	}
	// Exécution entière manquée entre deux relectures : on la signale quand même.
	if FinishedToast(prev, failed, on) == nil {
		t.Error("exécution vue directement terminée : toast attendu")
	}
	ok := okStatus(now)
	if FinishedToast(running, ok, on) != nil {
		t.Error("succès sans success dans toast_on")
	}
	if ts := FinishedToast(running, ok, []string{"success"}); ts == nil || ts.Level != LevelInfo {
		t.Errorf("succès avec success : %+v", ts)
	}
	dry := okStatus(now)
	dry.DryRun, dry.Result = true, state.ResultError
	if FinishedToast(running, dry, on) != nil {
		t.Error("pas de toast pour une simulation")
	}
}

func TestLateToast(t *testing.T) {
	v := Compute(Input{Status: okStatus(now.Add(-72 * time.Hour)), Config: cfg(), Now: now})
	if LateToast(v, time.Time{}, now) == nil {
		t.Fatal("retard : toast au démarrage")
	}
	if LateToast(v, now.Add(-time.Hour), now) != nil || LateToast(v, now.Add(-25*time.Hour), now) == nil {
		t.Fatal("retard : une fois par jour")
	}
}

func TestUpdateEntries(t *testing.T) {
	yesterday := now.Add(-11 * time.Hour)
	avail := selfupdate.Status{Available: &selfupdate.Available{Version: "0.11.0"}}
	base := Input{Status: okStatus(yesterday), Config: cfg(), User: "alice", Now: now, Version: "0.10.0"}

	// Compte standard : rien sur la mise à jour, même disponible.
	in := base
	in.Update = avail
	if v := Compute(in); v.UpdateLabel != "" || v.ShowCheckUpdate {
		t.Fatalf("compte standard : %+v", v)
	}
	// Administrateur, version plus récente notée : entrée active.
	in.Admin = true
	v := Compute(in)
	if v.UpdateTo != "0.11.0" || v.UpdateLabel != "Mettre à jour vers 0.11.0…" || !v.UpdateEnabled || v.ShowCheckUpdate {
		t.Fatalf("disponible : %+v", v)
	}
	// Pendant une sauvegarde : grisée.
	running := okStatus(yesterday)
	running.State = "running"
	in.Status = running
	if v := Compute(in); v.UpdateEnabled || !strings.Contains(v.UpdateLabel, "après la sauvegarde en cours") {
		t.Fatalf("pendant une sauvegarde : %+v", v)
	}
	// Version notée déjà installée (mise à jour faite entre-temps) : recherche proposée.
	in = base
	in.Admin, in.Update, in.Version = true, avail, "0.11.0"
	if v := Compute(in); v.UpdateLabel != "" || !v.ShowCheckUpdate {
		t.Fatalf("déjà installée : %+v", v)
	}
	// Mise à jour en cours : icône animée, infobulle, rien à lancer.
	in = base
	in.Admin, in.Update, in.Updating = true, avail, "téléchargement de la version 0.11.0"
	v = Compute(in)
	if v.Icon != IconRunning || !strings.Contains(v.Tooltip, "Mise à jour de l'agent : téléchargement") || v.UpdateEnabled || v.ShowCheckUpdate || v.RunNowEnabled {
		t.Fatalf("en cours : %+v", v)
	}
}

func TestUpdatedToast(t *testing.T) {
	last := &selfupdate.Record{From: "0.10.0", To: "0.11.0", At: now.Add(-time.Minute), Result: "ok"}
	if tt := UpdatedToast(selfupdate.Status{Last: last}, "0.11.0", now); tt == nil || !strings.Contains(tt.Text, "0.11.0") {
		t.Fatalf("après mise à jour : %+v", tt)
	}
	for name, c := range map[string]struct {
		rec     selfupdate.Record
		version string
	}{
		"ancienne":       {selfupdate.Record{To: "0.11.0", At: now.Add(-time.Hour), Result: "ok"}, "0.11.0"},
		"autre version":  {selfupdate.Record{To: "0.11.0", At: now, Result: "ok"}, "0.10.0"},
		"retour arrière": {selfupdate.Record{To: "0.10.0", At: now, Result: "rollback"}, "0.10.0"},
	} {
		r := c.rec
		if tt := UpdatedToast(selfupdate.Status{Last: &r}, c.version, now); tt != nil {
			t.Errorf("%s : %+v", name, tt)
		}
	}
}
