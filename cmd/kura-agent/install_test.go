package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/schedtask"
	"github.com/E-C-G/kura-agent/pkg/config"
)

type fakeSys struct {
	t        *testing.T
	calls    []string
	xml      string
	validate int
	asked    []string
	askErr   error
	lockErr  error
	legacy   bool // un compte svc-kura d'une version précédente existe
}

func (f *fakeSys) system(dir string) system {
	data := filepath.Join(dir, "data")
	return system{
		checkAdmin:    func() error { f.calls = append(f.calls, "admin"); return nil },
		secureDataDir: func(d string) error { f.calls = append(f.calls, "acl"); return os.MkdirAll(d, 0o755) },
		installBinary: func(d string) (string, string, error) {
			f.calls = append(f.calls, "binary")
			return filepath.Join(d, "kura-agent.exe"), filepath.Join(d, "kura-agentw.exe"), nil
		},
		setAutostart:    func(g string) error { f.calls = append(f.calls, "autostart"); return nil },
		removeAutostart: func() error { f.calls = append(f.calls, "rmautostart"); return nil },
		stopTrays:       func() int { f.calls = append(f.calls, "stoptrays"); return 1 },
		programDir:      func() string { return filepath.Join(dir, "prog") },
		legacyAccount:   func(n string) bool { return f.legacy },
		removeAccount:   func(n string) error { f.calls = append(f.calls, "rmaccount:"+n); return nil },
		accountSID:      func(n string) (string, error) { return "S-1-5-21-9-9-9-2000", nil },
		validate: func(c string) int {
			f.calls = append(f.calls, "validate")
			return f.validate
		},
		registerTask: func(x string) error { f.calls = append(f.calls, "task"); f.xml = x; return nil },
		deleteTask:   func() error { f.calls = append(f.calls, "rmtask"); return nil },
		removeProgramDir: func(d string) (bool, error) {
			f.calls = append(f.calls, "rmprog")
			return false, nil
		},
		acquireLock: func() (lock.Release, error) {
			f.calls = append(f.calls, "lock")
			return func() {}, f.lockErr
		},
		askSecret: func(p string) (string, error) {
			f.asked = append(f.asked, p)
			return "https://hc-ping.com/secret-uuid", f.askErr
		},
		askAWSKeys: func(n string) (string, error) {
			f.asked = append(f.asked, "aws:"+n)
			return "AKIDTEST:secret", f.askErr
		},
		now:        func() time.Time { return time.Date(2026, 10, 26, 9, 0, 0, 0, time.Local) },
		vault:      creds.Store{Dir: filepath.Join(data, "credentials")},
		dataDir:    data,
		configFile: filepath.Join(data, "config.yaml"),
		out:        &strings.Builder{},
	}
}

// currentUser existe sur toute machine de test (root, MACHINE\runneradmin…).
func currentUser(t *testing.T) string {
	u, err := user.Current()
	if err != nil {
		t.Skip("compte courant inconnu :", err)
	}
	return u.Username
}

func writeConfig(t *testing.T, dir, extra string) string {
	t.Helper()
	src := filepath.Join(dir, "source")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	y := fmt.Sprintf(`version: 1
profile:
  name: acme-srv01
  aws_credentials: credential:wasabi
  endpoint: https://s3.eu-central-1.wasabisys.com
  bucket: acme-srv01
schedule:
  type: daily
  time: "22:30"
  days: [mon, fri]
  run_if_missed: true
jobs:
  - id: data
    source: %q
    destination: DATA
tray:
  allow_run_now: [%q]
notify:
  report:
    type: healthchecks
    url: credential:hc-url
%s`, filepath.ToSlash(src), currentUser(t), extra)
	p := filepath.Join(dir, "new.yaml")
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallFresh(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t}
	sys := f.system(dir)
	cfg := writeConfig(t, dir, "")
	if code := install(sys, cfg, false); code != 0 {
		t.Fatalf("code %d ; appels %v", code, f.calls)
	}
	want := "admin acl binary validate task autostart"
	if got := strings.Join(f.calls, " "); got != want {
		t.Fatalf("ordre des étapes :\n reçu   %s\n attendu %s", got, want)
	}
	if len(f.asked) != 2 || f.asked[0] != "aws:wasabi" {
		t.Fatalf("saisies : %v", f.asked)
	}
	for name, want := range map[string]string{"wasabi": "AKIDTEST:secret", "hc-url": "https://hc-ping.com/secret-uuid"} {
		if v, err := sys.vault.Get(name); err != nil || v != want {
			t.Errorf("coffre %s = %q, %v", name, v, err)
		}
	}
	if b, err := os.ReadFile(sys.configFile); err != nil || !strings.Contains(string(b), "acme-srv01") {
		t.Fatalf("configuration non copiée : %v", err)
	}
	for _, w := range []string{"<UserId>S-1-5-18</UserId>", "(A;;FRFX;;;S-1-5-21-9-9-9-2000)",
		"kura-agent.exe</Command>", "<Arguments>run --quiet --scheduled</Arguments>", "<Monday></Monday>"} {
		if !strings.Contains(f.xml, w) {
			t.Errorf("%q absent de la tâche", w)
		}
	}
	if strings.Contains(f.xml, "secret") {
		t.Error("un secret apparaît dans la définition de la tâche")
	}

	// Rejouer l'installation : secrets déjà là, rien à redemander, pas de sauvegarde de config.
	f2 := &fakeSys{t: t}
	sys2 := f2.system(dir)
	if code := install(sys2, cfg, false); code != 0 || len(f2.asked) != 0 {
		t.Fatalf("réinstallation : code %d, saisies %v", code, f2.asked)
	}
	if m, _ := filepath.Glob(sys.configFile + ".*.bak"); len(m) != 0 {
		t.Fatalf("sauvegarde inutile : %v", m)
	}
	// Configuration en place mais illisible : jamais écrasée sans copie.
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		os.Chmod(sys.configFile, 0)
		if _, err := installConfig(cfg, sys.configFile, time.Now()); err == nil {
			t.Fatal("configuration illisible écrasée")
		}
		os.Chmod(sys.configFile, 0o644)
	}
	// Configuration modifiée : l'ancienne est gardée.
	cfg = writeConfig(t, dir, "logging:\n  level: debug\n")
	if code := install(f2.system(dir), cfg, false); code != 0 {
		t.Fatalf("mise à jour : code %d", code)
	}
	if m, _ := filepath.Glob(sys.configFile + ".*.bak"); len(m) != 1 {
		t.Fatalf("ancienne configuration non gardée : %v", m)
	}
}

func TestInstallStopsWhenValidateFails(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t, validate: 3}
	if code := install(f.system(dir), writeConfig(t, dir, ""), false); code == 0 {
		t.Fatal("install réussi malgré validate en échec")
	}
	if f.xml != "" || strings.Contains(strings.Join(f.calls, " "), "task") {
		t.Fatalf("tâche créée malgré validate en échec : %v", f.calls)
	}
	// --skip-validate passe outre.
	f = &fakeSys{t: t, validate: 3}
	if code := install(f.system(dir), writeConfig(t, dir, ""), true); code != 0 || f.xml == "" {
		t.Fatalf("--skip-validate : code %d", code)
	}
}

func TestInstallStopsWhenSecretMissing(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t, askErr: errors.New("EOF")}
	if code := install(f.system(dir), writeConfig(t, dir, ""), false); code == 0 {
		t.Fatal("install réussi sans secret")
	}
	if got := strings.Join(f.calls, " "); strings.Contains(got, "validate") || strings.Contains(got, "task") {
		t.Fatalf("étapes après un secret manquant : %v", got)
	}
}

func TestInstallBadConfigTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t}
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("version: 1\n"), 0o644)
	if code := install(f.system(dir), bad, false); code != exitInstallConfig {
		t.Fatalf("code %d", code)
	}
	if got := strings.Join(f.calls, " "); got != "admin" {
		t.Fatalf("étapes malgré une config invalide : %v", got)
	}
}

func TestUninstall(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t}
	sys := f.system(dir)
	os.MkdirAll(sys.dataDir, 0o755)
	f.legacy = true
	if code := uninstall(sys, false); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := strings.Join(f.calls, " "); got != "admin lock rmautostart stoptrays rmtask rmaccount:svc-kura rmprog" {
		t.Fatalf("étapes : %s", got)
	}
	if _, err := os.Stat(sys.dataDir); err != nil {
		t.Fatal("données effacées sans --purge")
	}
	f = &fakeSys{t: t}
	if code := uninstall(f.system(dir), true); code != 0 {
		t.Fatalf("code %d", code)
	}
	if strings.Contains(strings.Join(f.calls, " "), "rmaccount") {
		t.Fatal("aucun ancien compte : rien à supprimer")
	}
	if _, err := os.Stat(sys.dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("--purge n'a pas effacé les données")
	}
	f = &fakeSys{t: t, lockErr: lock.ErrBusy}
	if code := uninstall(f.system(dir), false); code == 0 || strings.Contains(strings.Join(f.calls, " "), "rmtask") {
		t.Fatalf("désinstallation pendant une sauvegarde : code %d, %v", code, f.calls)
	}
}

func TestDescribeSchedule(t *testing.T) {
	for _, c := range []struct {
		s    config.Schedule
		want string
	}{
		{config.Schedule{Type: "daily", Time: "22:30", Days: []string{"mon", "fri"}, RunIfMissed: true}, "à 22:30 (lun, ven), rattrapage si manquée"},
		{config.Schedule{Type: "daily", Time: "01:00"}, "à 01:00 tous les jours"},
		{config.Schedule{Type: "hourly"}, "toutes les heures tous les jours"},
	} {
		if got := describeSchedule(c.s); got != c.want {
			t.Errorf("%+v : %q, attendu %q", c.s, got, c.want)
		}
	}
}

func TestInstallFallsBackWithoutRunNowRights(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t}
	sys := f.system(dir)
	sys.registerTask = func(x string) error {
		return &schedtask.RunNowNotAppliedError{Cause: "Accès refusé"}
	}
	out := &strings.Builder{}
	sys.out = out
	if code := install(sys, writeConfig(t, dir, ""), false); code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out.String(), "que pour les administrateurs") {
		t.Fatalf("avertissement absent :\n%s", out)
	}
	if b, err := os.ReadFile(filepath.Join(sys.dataDir, "task.xml")); err != nil || !strings.Contains(fromUTF16(b), "<UserId>S-1-5-18</UserId>") {
		t.Fatalf("task.xml : %v", err)
	}
}

func TestInstallRemovesLegacyAccount(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t, legacy: true}
	if code := install(f.system(dir), writeConfig(t, dir, ""), false); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := strings.Join(f.calls, " "); got != "admin acl binary validate task rmaccount:svc-kura autostart" {
		t.Fatalf("étapes : %s", got)
	}
}

func fromUTF16(b []byte) string {
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		return ""
	}
	u := make([]uint16, (len(b)-2)/2)
	for i := range u {
		u[i] = uint16(b[2+2*i]) | uint16(b[3+2*i])<<8
	}
	return string(utf16.Decode(u))
}
