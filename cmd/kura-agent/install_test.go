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

	"github.com/E-C-G/kura-agent/internal/awsinstall"
	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/internal/launchd"
	"github.com/E-C-G/kura-agent/internal/lock"
	"github.com/E-C-G/kura-agent/internal/schedtask"
	"github.com/E-C-G/kura-agent/internal/systemd"
	"github.com/E-C-G/kura-agent/pkg/config"
	"github.com/E-C-G/kura-agent/pkg/pack"
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
	awsErr   error
	awsTemp  string
	units    systemd.Units
	plists   launchd.Plists
	msis     []string
	removed  []string
	password string
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
			if f.password != "" && strings.Contains(p, "paquet") {
				return f.password, nil
			}
			return "https://hc-ping.com/secret-uuid", f.askErr
		},
		ensureAWS: func(tmp, want string, c []string) (awsinstall.Result, error) {
			f.calls = append(f.calls, "awscli")
			f.msis = c
			f.awsTemp = tmp
			return awsinstall.Result{Path: `C:\Program Files\Amazon\AWSCLIV2\aws.exe`, Version: "aws-cli/2.31.0", Installed: true, Signer: "Amazon.com Services LLC"}, f.awsErr
		},
		remove: func(p string) error { f.removed = append(f.removed, p); return os.Remove(p) },
		askAWSKeys: func(n string) (string, error) {
			f.asked = append(f.asked, "aws:"+n)
			return "AKIDTEST:secret", f.askErr
		},
		now:        func() time.Time { return time.Date(2026, 10, 26, 9, 0, 0, 0, time.Local) },
		vault:      creds.Store{Dir: filepath.Join(data, "credentials")},
		dataDir:    data,
		configFile: filepath.Join(data, "config.yaml"),
		awsWorkDir: filepath.Join(data, "awscli"),
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
	if code := install(sys, installOptions{cfgPath: cfg, skipValidate: false}); code != 0 {
		t.Fatalf("code %d ; appels %v", code, f.calls)
	}
	want := "admin acl awscli binary validate task autostart"
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
	if code := install(sys2, installOptions{cfgPath: cfg, skipValidate: false}); code != 0 || len(f2.asked) != 0 {
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
	if code := install(f2.system(dir), installOptions{cfgPath: cfg, skipValidate: false}); code != 0 {
		t.Fatalf("mise à jour : code %d", code)
	}
	if m, _ := filepath.Glob(sys.configFile + ".*.bak"); len(m) != 1 {
		t.Fatalf("ancienne configuration non gardée : %v", m)
	}
}

func TestInstallStopsWhenValidateFails(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t, validate: 3}
	if code := install(f.system(dir), installOptions{cfgPath: writeConfig(t, dir, ""), skipValidate: false}); code == 0 {
		t.Fatal("install réussi malgré validate en échec")
	}
	if f.xml != "" || strings.Contains(strings.Join(f.calls, " "), "task") {
		t.Fatalf("tâche créée malgré validate en échec : %v", f.calls)
	}
	// --skip-validate passe outre.
	f = &fakeSys{t: t, validate: 3}
	if code := install(f.system(dir), installOptions{cfgPath: writeConfig(t, dir, ""), skipValidate: true}); code != 0 || f.xml == "" {
		t.Fatalf("--skip-validate : code %d", code)
	}
}

func TestInstallStopsWhenSecretMissing(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t, askErr: errors.New("EOF")}
	if code := install(f.system(dir), installOptions{cfgPath: writeConfig(t, dir, ""), skipValidate: false}); code == 0 {
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
	if code := install(f.system(dir), installOptions{cfgPath: bad, skipValidate: false}); code != exitInstallConfig {
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
	if code := install(sys, installOptions{cfgPath: writeConfig(t, dir, ""), skipValidate: false}); code != 0 {
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
	if code := install(f.system(dir), installOptions{cfgPath: writeConfig(t, dir, ""), skipValidate: false}); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := strings.Join(f.calls, " "); got != "admin acl awscli binary validate task rmaccount:svc-kura autostart" {
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

// writePackage scelle la configuration de writeConfig dans un paquet d'installation.
func writePackage(t *testing.T, dir string, edit func(*pack.Contents)) (path, password string) {
	t.Helper()
	y, err := os.ReadFile(writeConfig(t, dir, ""))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 8, 0, 0, 0, time.Local)
	c := pack.Contents{Kind: pack.KindInstall, CreatedAt: now, ExpiresAt: now.Add(72 * time.Hour),
		Client: "acme", Host: "SRV01", Bucket: "acme-srv01", Config: string(y),
		Credentials: []pack.Credential{
			{Name: "wasabi", Kind: "aws", AccessKeyID: "AKIDPAQUET", SecretAccessKey: "secretpaquet"},
			{Name: "hc-url", Kind: "secret", Value: "https://hc-ping.example/paquet"},
		}}
	if edit != nil {
		edit(&c)
	}
	password = "K7QM-2XRT-9HWB-P4NC-VJ3F"
	data, err := pack.Seal(c, password, 1000)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "acme-srv01.kura")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, password
}

func TestInstallFromPackage(t *testing.T) {
	dir := t.TempDir()
	file, pw := writePackage(t, dir, nil)
	f := &fakeSys{t: t, password: pw}
	sys := f.system(dir)
	msi := filepath.Join(dir, awsinstall.MSIName)
	if code := install(sys, installOptions{pkgPath: file, msiCandidates: []string{msi}}); code != 0 {
		t.Fatalf("code %d ; appels %v", code, f.calls)
	}
	if got := strings.Join(f.calls, " "); got != "admin acl awscli binary validate task autostart" {
		t.Fatalf("étapes : %s", got)
	}
	if len(f.asked) != 1 || len(f.msis) != 1 || f.msis[0] != msi || f.awsTemp != filepath.Join(sys.dataDir, "awscli") {
		t.Fatalf("saisies %v, MSI %v, dossier %s", f.asked, f.msis, f.awsTemp)
	}
	for name, want := range map[string]string{"wasabi": "AKIDPAQUET:secretpaquet", "hc-url": "https://hc-ping.example/paquet"} {
		if v, err := sys.vault.Get(name); err != nil || v != want {
			t.Errorf("coffre %s = %q, %v", name, v, err)
		}
	}
	if b, err := os.ReadFile(sys.configFile); err != nil || !strings.Contains(string(b), "bucket: acme-srv01") {
		t.Fatalf("configuration du paquet non installée : %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("le paquet doit être supprimé après installation")
	}
}

func TestInstallPackageRefusals(t *testing.T) {
	cases := map[string]func(*pack.Contents){
		"clé seule": func(c *pack.Contents) { c.Kind = pack.KindCredentials; c.Config = "" },
		"expiré":    func(c *pack.Contents) { c.ExpiresAt = c.CreatedAt.Add(30 * time.Minute) },
		"bucket":    func(c *pack.Contents) { c.Bucket = "autre-bucket" },
		"clé absente": func(c *pack.Contents) {
			c.Credentials[0].Name = "wasabi-2"
		},
		"configuration invalide": func(c *pack.Contents) { c.Config = "version: 1\nprofile: {}\n" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file, pw := writePackage(t, dir, edit)
			f := &fakeSys{t: t, password: pw}
			sys := f.system(dir)
			if code := install(sys, installOptions{pkgPath: file, keep: false}); code == 0 {
				t.Fatal("installé")
			}
			if strings.Contains(strings.Join(f.calls, " "), "acl") {
				t.Fatalf("modifications avant refus : %v", f.calls)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("paquet supprimé malgré l'échec")
			}
		})
	}
}

func TestInstallPackageWrongPassword(t *testing.T) {
	dir := t.TempDir()
	file, _ := writePackage(t, dir, nil)
	f := &fakeSys{t: t, password: "AAAA-BBBB-CCCC-DDDD-EEEE"}
	if code := install(f.system(dir), installOptions{pkgPath: file}); code == 0 || len(f.asked) != 3 {
		t.Fatalf("code %d, saisies %v", code, f.asked)
	}
}

func TestInstallStopsWhenAWSCLIFails(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t, awsErr: errors.New("signature HashMismatch")}
	if code := install(f.system(dir), installOptions{cfgPath: writeConfig(t, dir, "")}); code == 0 || f.xml != "" {
		t.Fatalf("code %d, appels %v", code, f.calls)
	}
	// Chemin explicite : jamais d'installation automatique.
	f = &fakeSys{t: t}
	aws := filepath.Join(dir, "aws.exe")
	os.WriteFile(aws, nil, 0o755)
	cfg := writeConfig(t, dir, "")
	b, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, []byte(strings.Replace(string(b), "profile:\n", "profile:\n  aws_cli: "+filepath.ToSlash(aws)+"\n", 1)), 0o644)
	if code := install(f.system(dir), installOptions{cfgPath: cfg}); code != 0 || strings.Contains(strings.Join(f.calls, " "), "awscli") {
		t.Fatalf("code %d, appels %v", code, f.calls)
	}
}

func TestInstallPackageRestoresSecretsWhenValidateFails(t *testing.T) {
	dir := t.TempDir()
	file, pw := writePackage(t, dir, nil)
	f := &fakeSys{t: t, password: pw, validate: 3}
	sys := f.system(dir)
	if err := sys.vault.Init(); err != nil {
		t.Fatal(err)
	}
	sys.vault.Set("wasabi", "AKIDANCIENNE:ancien")
	if code := install(sys, installOptions{pkgPath: file}); code == 0 {
		t.Fatal("installé malgré validate")
	}
	if v, err := sys.vault.Get("wasabi"); err != nil || v != "AKIDANCIENNE:ancien" {
		t.Fatalf("ancienne clé non remise : %q %v", v, err)
	}
	if _, err := sys.vault.Get("hc-url"); err == nil {
		t.Fatal("secret ajouté par le paquet resté dans le coffre")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("paquet supprimé malgré l'échec")
	}
}

// linuxSystem : même faux système, version Linux (compte de service, timer systemd).
func (f *fakeSys) linuxSystem(dir string) system {
	sys := f.system(dir)
	sys.linux = true
	sys.ensureAccount = func() (bool, error) { f.calls = append(f.calls, "account"); return true, nil }
	sys.installTimer = func(u systemd.Units) error { f.calls = append(f.calls, "timer"); f.units = u; return nil }
	sys.registerTask = func(string) error { f.t.Fatal("tâche Windows sous Linux"); return nil }
	sys.installBinary = func(string) (string, string, error) {
		f.calls = append(f.calls, "binary")
		return "/usr/local/bin/kura-agent", "", nil
	}
	sys.removeBinary = func() (string, error) { f.calls = append(f.calls, "rmbinary"); return "/usr/local/bin/kura-agent", nil }
	sys.programDir = func() string { return "" }
	sys.legacyAccount = func(string) bool { return false }
	sys.awsWorkDir = filepath.Join(dir, "cache", "kura-install")
	sys.purgeDirs = []string{filepath.Join(dir, "etc-kura"), filepath.Join(dir, "log-kura")}
	return sys
}

func TestInstallLinux(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t}
	sys := f.linuxSystem(dir)
	out := sys.out.(*strings.Builder)
	cfg := writeConfig(t, dir, "")
	if code := install(sys, installOptions{cfgPath: cfg}); code != 0 {
		t.Fatalf("code %d ; appels %v", code, f.calls)
	}
	if got := strings.Join(f.calls, " "); got != "admin account acl awscli binary validate timer" {
		t.Fatalf("étapes : %s", got)
	}
	if f.awsTemp != sys.awsWorkDir {
		t.Fatalf("dossier de travail de la CLI AWS : %s", f.awsTemp)
	}
	for _, w := range []string{"ExecStart=/usr/local/bin/kura-agent run --scheduled", "User=kura-agent", "ImportCredential=kura.*"} {
		if !strings.Contains(f.units.Service, w) {
			t.Errorf("service sans %q", w)
		}
	}
	if !strings.Contains(f.units.Timer, "OnCalendar=Mon,Fri *-*-* 22:30:00") || !strings.Contains(f.units.Timer, "Persistent=true") {
		t.Errorf("timer :\n%s", f.units.Timer)
	}
	text := out.String()
	for _, w := range []string{"compte de service kura-agent créé", "timer kura-sync.timer actif", "systemctl start kura-sync.service",
		"tray.allow_run_now ignoré sous Linux"} {
		if !strings.Contains(text, w) {
			t.Errorf("sortie sans %q :\n%s", w, text)
		}
	}
	for _, w := range []string{"schtasks", "kura-agentw.exe", "SYSTEM"} {
		if strings.Contains(text, w) {
			t.Errorf("sortie Windows sous Linux (%q) :\n%s", w, text)
		}
	}
	if b, err := os.ReadFile(sys.configFile); err != nil || !strings.Contains(string(b), "acme-srv01") {
		t.Fatalf("configuration non installée : %v", err)
	}
}

func TestInstallLinuxValidateFails(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t, validate: 3}
	if code := install(f.linuxSystem(dir), installOptions{cfgPath: writeConfig(t, dir, "")}); code == 0 || strings.Contains(strings.Join(f.calls, " "), "timer") {
		t.Fatalf("code %d, appels %v", code, f.calls)
	}
}

func TestUninstallLinux(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t}
	sys := f.linuxSystem(dir)
	sys.deleteTask = func() error { f.calls = append(f.calls, "rmtimer"); return nil }
	if err := sys.vault.Init(); err != nil {
		t.Fatal(err)
	}
	sys.vault.Set("wasabi", "AKID:secret")
	for _, d := range append([]string{sys.dataDir}, sys.purgeDirs...) {
		os.MkdirAll(d, 0o755)
	}
	if code := uninstall(sys, true); code != 0 {
		t.Fatalf("code %d, appels %v", code, f.calls)
	}
	if got := strings.Join(f.calls, " "); got != "admin lock rmautostart stoptrays rmtimer rmbinary" {
		t.Fatalf("étapes : %s", got)
	}
	if names, _ := sys.vault.List(); len(names) != 0 {
		t.Fatalf("secrets restés : %v", names)
	}
	for _, d := range append([]string{sys.dataDir}, sys.purgeDirs...) {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s resté", d)
		}
	}
}

// macSystem : même faux système, version macOS (démons launchd sous root).
func (f *fakeSys) macSystem(dir string) system {
	sys := f.system(dir)
	sys.darwin = true
	sys.installDaemons = func(p launchd.Plists) error { f.calls = append(f.calls, "daemons"); f.plists = p; return nil }
	sys.registerTask = func(string) error { f.t.Fatal("tâche Windows sous macOS"); return nil }
	sys.installBinary = func(string) (string, string, error) {
		f.calls = append(f.calls, "binary")
		return "/usr/local/bin/kura-agent", "", nil
	}
	sys.removeBinary = func() (string, error) { f.calls = append(f.calls, "rmbinary"); return "/usr/local/bin/kura-agent", nil }
	sys.removeVaultKey = func() error { f.calls = append(f.calls, "rmkey"); return nil }
	sys.programDir = func() string { return "" }
	sys.legacyAccount = func(string) bool { return false }
	sys.logsDir = filepath.Join(dir, "Logs", "Kura")
	sys.purgeDirs = []string{sys.logsDir}
	return sys
}

func TestInstallMac(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t}
	sys := f.macSystem(dir)
	out := sys.out.(*strings.Builder)
	cfg := writeConfig(t, dir, "")
	if code := install(sys, installOptions{cfgPath: cfg}); code != 0 {
		t.Fatalf("code %d ; appels %v", code, f.calls)
	}
	if got := strings.Join(f.calls, " "); got != "admin acl awscli binary validate daemons" {
		t.Fatalf("étapes : %s", got)
	}
	sync := string(f.plists.Sync)
	for _, w := range []string{"<string>/usr/local/bin/kura-agent</string>", "<string>--scheduled</string>", "<key>Weekday</key>", sys.logsDir} {
		if !strings.Contains(sync, w) {
			t.Errorf("démon sans %q :\n%s", w, sync)
		}
	}
	if strings.Contains(sync, "UserName") {
		t.Error("démon sous un autre compte que root")
	}
	if !strings.Contains(string(f.plists.CatchUp), "--catch-up") {
		t.Errorf("rattrapage absent (run_if_missed) :\n%s", f.plists.CatchUp)
	}
	text := out.String()
	for _, w := range []string{"démon " + launchd.Label + " actif", "compte root", "Accès\ncomplet au disque", "launchctl kickstart system/" + launchd.Label,
		"tray.allow_run_now ignoré sous macOS", "trousseau Système"} {
		if !strings.Contains(text, w) {
			t.Errorf("sortie sans %q :\n%s", w, text)
		}
	}
	for _, w := range []string{"schtasks", "kura-agentw.exe", "systemctl", "SYSTEM"} {
		if strings.Contains(text, w) {
			t.Errorf("sortie d'un autre système sous macOS (%q) :\n%s", w, text)
		}
	}
}

func TestInstallMacValidateFails(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t, validate: 3}
	if code := install(f.macSystem(dir), installOptions{cfgPath: writeConfig(t, dir, "")}); code == 0 || strings.Contains(strings.Join(f.calls, " "), "daemons") {
		t.Fatalf("code %d, appels %v", code, f.calls)
	}
}

func TestUninstallMac(t *testing.T) {
	dir := t.TempDir()
	f := &fakeSys{t: t}
	sys := f.macSystem(dir)
	sys.deleteTask = func() error { f.calls = append(f.calls, "rmdaemons"); return nil }
	if err := sys.vault.Init(); err != nil {
		t.Fatal(err)
	}
	sys.vault.Set("wasabi", "AKID:secret")
	for _, d := range append([]string{sys.dataDir}, sys.purgeDirs...) {
		os.MkdirAll(d, 0o755)
	}
	if code := uninstall(sys, true); code != 0 {
		t.Fatalf("code %d, appels %v", code, f.calls)
	}
	if got := strings.Join(f.calls, " "); got != "admin lock rmautostart stoptrays rmdaemons rmbinary rmkey" {
		t.Fatalf("étapes : %s", got)
	}
	if !strings.Contains(sys.out.(*strings.Builder).String(), "démons "+launchd.Label) {
		t.Fatal(sys.out)
	}
	for _, d := range append([]string{sys.dataDir}, sys.purgeDirs...) {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s resté", d)
		}
	}
}
