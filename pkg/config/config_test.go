package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeEnv struct {
	vars     map[string]string
	profiles map[string]string
	accounts map[string]bool
}

func (f fakeEnv) LookupEnv(n string) (string, bool) { v, ok := f.vars[n]; return v, ok }
func (f fakeEnv) ProfileDir(a string) (string, error) {
	if d, ok := f.profiles[a]; ok {
		return d, nil
	}
	return "", errors.New("inconnu")
}
func (f fakeEnv) AccountExists(a string) bool { return f.accounts[a] }

func testEnv() fakeEnv {
	return fakeEnv{
		vars:     map[string]string{"USERNAME": "svc-kura"},
		profiles: map[string]string{"alice": `C:\Users\alice`},
		accounts: map[string]bool{"alice": true, "admin": true},
	}
}

func TestExampleConfigLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "configs", "example.yaml"), LoadOptions{Env: testEnv()})
	if err != nil {
		t.Fatalf("la config d'exemple doit être valide : %v", err)
	}
	if len(c.Jobs) != 2 {
		t.Fatalf("2 jobs attendus, reçu %d", len(c.Jobs))
	}
	out, _ := c.Job("outlook")
	if want := "C:/Users/alice/AppData/Local/Microsoft/Outlook"; out.Source != want {
		t.Errorf("source outlook = %q, attendu %q", out.Source, want)
	}
	if c.PreTasks[0].GracefulTimeout.Duration != 60*time.Second {
		t.Errorf("graceful_timeout mal lu : %v", c.PreTasks[0].GracefulTimeout)
	}
	if c.Schedule.MaxDuration.Duration != 6*time.Hour {
		t.Errorf("max_duration mal lu : %v", c.Schedule.MaxDuration)
	}
	data, _ := c.Job("data")
	if data.Guard == nil || data.Guard.MaxDeletes != 500 {
		t.Errorf("garde-fou du job data mal lu : %+v", data.Guard)
	}
}

func TestDefaultGuardWhenDeleteWithoutGuard(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "configs", "example.yaml"), LoadOptions{Env: testEnv()})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := c.Job("outlook")
	if out.Guard == nil || out.Guard.MinSourceFiles != 1 {
		t.Fatalf("un job avec delete doit recevoir un garde-fou par défaut, reçu %+v", out.Guard)
	}
}

func TestExpandString(t *testing.T) {
	env := testEnv()
	cases := []struct {
		in, want, err string
	}{
		{"${PROFILE:alice}/AppData", "C:/Users/alice/AppData", ""},
		{"C/${USERNAME}/x", "C/svc-kura/x", ""},
		{"sans variable", "sans variable", ""},
		{"${PROFILE}", "", "précisez le compte"},
		{"${PROFILE:inconnu}/x", "", "profil introuvable"},
		{"${ABSENTE}", "", "variable d'environnement absente"},
		{"${USERNAME:x}", "", "seule ${PROFILE:compte}"},
	}
	for _, tc := range cases {
		got, err := ExpandString(tc.in, env)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q : erreur contenant %q attendue, reçu %v", tc.in, tc.err, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q : attendu %q, reçu %q (%v)", tc.in, tc.want, got, err)
		}
	}
}

const minimal = `
version: 1
profile:
  endpoint: https://s3.eu-central-1.wasabisys.com
  bucket: acme-srv01
schedule:
  type: daily
  time: "22:30"
jobs:
  - id: data
    source: "%s"
    destination: DATA
`

func parseWith(t *testing.T, yaml string, opt LoadOptions) error {
	t.Helper()
	if opt.Env == nil {
		opt.Env = testEnv()
	}
	_, err := Parse([]byte(yaml), opt)
	return err
}

func TestValidationCollectsAllErrors(t *testing.T) {
	bad := `
version: 2
profile:
  endpoint: http://s3.example.com
  bucket: Bad_Bucket
  checksum_algorithm: MD5
schedule:
  type: monthly
jobs:
  - id: Data
    source: ""
    destination: /DATA
  - id: a
    source: x
    destination: C/alice
  - id: b
    source: y
    destination: C/alice/Outlook
    depends_on_pre_task: outlook.exe
notify:
  webhook:
    url: https://hook.example/abc
    token: secret-en-clair
`
	err := parseWith(t, bad, LoadOptions{})
	var ve ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("ValidationErrors attendu, reçu %v", err)
	}
	wantFields := []string{
		"version", "profile.bucket", "profile.endpoint", "profile.checksum_algorithm",
		"schedule.type", "jobs[0].id", "jobs[0].source", "jobs[0].destination",
		"jobs[2].destination", "jobs[2].depends_on_pre_task", "notify.webhook.token",
	}
	got := map[string]bool{}
	for _, fe := range ve {
		got[fe.Field] = true
	}
	for _, f := range wantFields {
		if !got[f] {
			t.Errorf("erreur attendue sur %s ; erreurs reçues :\n%v", f, err)
		}
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	y := strings.Replace(minimal, "destination: DATA", "destination: DATA\n    delet: true", 1)
	err := parseWith(t, strings.Replace(y, "%s", "E:/", 1), LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "delet") {
		t.Fatalf("une faute de frappe doit être refusée, reçu %v", err)
	}
}

func TestCheckSystemVerifiesSourceAndAccounts(t *testing.T) {
	dir := t.TempDir()
	ok := strings.Replace(minimal, "%s", filepath.ToSlash(dir), 1)
	if err := parseWith(t, ok, LoadOptions{CheckSystem: true}); err != nil {
		t.Fatalf("source existante refusée : %v", err)
	}
	missing := strings.Replace(minimal, "%s", filepath.ToSlash(filepath.Join(dir, "absent")), 1)
	if err := parseWith(t, missing, LoadOptions{CheckSystem: true}); err == nil {
		t.Fatal("une source absente doit être refusée avec CheckSystem")
	}
	file := filepath.Join(dir, "f.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	notDir := strings.Replace(minimal, "%s", filepath.ToSlash(file), 1)
	if err := parseWith(t, notDir, LoadOptions{CheckSystem: true}); err == nil {
		t.Fatal("une source qui n'est pas un dossier doit être refusée")
	}
	withTray := ok + "tray:\n  users: [alice, fantome]\n"
	err := parseWith(t, withTray, LoadOptions{CheckSystem: true})
	if err == nil || !strings.Contains(err.Error(), "fantome") {
		t.Fatalf("un compte inexistant doit être signalé, reçu %v", err)
	}
	// Sans CheckSystem (usage kura-ctl), ni la source ni les comptes ne sont vérifiés.
	if err := parseWith(t, strings.Replace(withTray, filepath.ToSlash(dir), "Z:/ailleurs", 1), LoadOptions{}); err != nil {
		t.Fatalf("sans CheckSystem, rien ne doit être vérifié sur la machine : %v", err)
	}
}

func TestNested(t *testing.T) {
	cases := map[[2]string]bool{
		{"DATA", "DATA"}:               true,
		{"C/alice", "C/alice/Outlook"}: true,
		{"C/alice/Outlook", "C/alice"}: true,
		{"C/alice", "C/jjx"}:           false,
		{"DATA", "C/alice/Outlook"}:    false,
	}
	for in, want := range cases {
		if got := nested(in[0], in[1]); got != want {
			t.Errorf("nested(%q, %q) = %v, attendu %v", in[0], in[1], got, want)
		}
	}
}
