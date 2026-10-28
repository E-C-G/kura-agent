package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const withPrepare = `
version: 1
profile:
  endpoint: https://s3.eu-central-1.wasabisys.com
  bucket: acme-srv01
schedule:
  type: daily
  time: "22:30"
tools:
  zip: "C:/Program Files/7-Zip/7z.exe"
prepare:
  - id: courrier
    type: zip
    source: "${PROFILE:alice}/Courrier"
    exclude: ["*.tmp"]
jobs:
  - id: courrier
    source: "${STAGING:courrier}"
    destination: COURRIER
    delete: true
`

func TestPrepareZipLoads(t *testing.T) {
	c, err := Parse([]byte(withPrepare), LoadOptions{Env: testEnv(), StagingDir: `C:\ProgramData\Kura\staging`})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.PrepareByID("courrier")
	if !ok || p.Source != "C:/Users/alice/Courrier" || p.Level != LevelDeflate || p.OnLocked != OnLockedFail || p.Timeout.Duration != time.Hour {
		t.Fatalf("préparation mal lue : %+v", p)
	}
	j, _ := c.Job("courrier")
	if j.Prepare != "courrier" || j.Source != "C:/ProgramData/Kura/staging/courrier" {
		t.Fatalf("source ${STAGING} mal résolue : %+v", j)
	}
	// Sans zone de préparation (kura-ctl), la source reste telle quelle.
	c, err = Parse([]byte(withPrepare), LoadOptions{Env: testEnv()})
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := c.Job("courrier"); j.Source != "${STAGING:courrier}" || j.Prepare != "courrier" {
		t.Fatalf("source ${STAGING} modifiée sans zone : %+v", j)
	}
}

// La source d'un job tiré d'une préparation n'existe pas avant l'exécution : CheckSystem
// ne la vérifie pas ; celle de la préparation, si.
func TestPrepareCheckSystem(t *testing.T) {
	dir := t.TempDir()
	env := testEnv()
	env.profiles = map[string]string{"alice": dir}
	yaml := withPrepare
	opt := LoadOptions{Env: env, CheckSystem: true, StagingDir: filepath.Join(dir, "absent")}
	if err := parseWith(t, yaml, opt); err == nil || !strings.Contains(err.Error(), "prepare[0].source") {
		t.Fatalf("source de préparation absente acceptée : %v", err)
	}
	os.Mkdir(filepath.Join(dir, "Courrier"), 0o755)
	if err := parseWith(t, yaml, opt); err != nil {
		t.Fatalf("zone de préparation absente refusée : %v", err)
	}
}

func TestPrepareValidation(t *testing.T) {
	cases := []struct {
		name, from, to, field string
	}{
		{"id inconnu", "${STAGING:courrier}", "${STAGING:autre}", "jobs[0].source"},
		{"préparation inutilisée", "${STAGING:courrier}", "D:/Donnees", "prepare[0].id"},
		{"tools.zip absent", "  zip: \"C:/Program Files/7-Zip/7z.exe\"\n", "", "prepare[0].type"},
		{"tools.zip relatif", "C:/Program Files/7-Zip/7z.exe", "7z.exe", "tools.zip"},
		{"tools.zip inconnu", "C:/Program Files/7-Zip/7z.exe", "C:/Outils/winrar.exe", "tools.zip"},
		{"type à venir", "type: zip", "type: gpg", "prepare[0].type"},
		{"type inconnu", "type: zip", "type: rar", "prepare[0].type"},
		{"niveau", "exclude:", "level: max\n    exclude:", "prepare[0].level"},
		{"on_locked", "exclude:", "on_locked: ignore\n    exclude:", "prepare[0].on_locked"},
		{"pré-tâche inconnue", "exclude:", "depends_on_pre_task: outlook.exe\n    exclude:", "prepare[0].depends_on_pre_task"},
		{"id invalide", "- id: courrier\n    type", "- id: Courrier\n    type", "prepare[0].id"},
		{"STAGING ailleurs", "destination: COURRIER", "destination: \"${STAGING:courrier}\"", "jobs[0].destination"},
		{"source relative", "${PROFILE:alice}/Courrier", "Courrier", "prepare[0].source"},
		{"id réservé", "- id: courrier\n    type", "- id: nul\n    type", "prepare[0].id"},
		{"id de job réservé", "- id: courrier\n    source", "- id: com1\n    source", "jobs[0].id"},
	}
	for _, c := range cases {
		yaml := strings.Replace(withPrepare, c.from, c.to, 1)
		err := parseWith(t, yaml, LoadOptions{})
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s : erreur sur %s attendue, reçu %v", c.name, c.field, err)
		}
	}
	// Un outil Unix est accepté par une configuration lue sous Windows, et l'inverse :
	// kura-ctl prépare des configurations pour d'autres systèmes.
	yaml := strings.Replace(withPrepare, "C:/Program Files/7-Zip/7z.exe", "/usr/bin/zip", 1)
	if err := parseWith(t, yaml, LoadOptions{}); err != nil {
		t.Errorf("outil Unix refusé : %v", err)
	}
}

func TestZipFlavor(t *testing.T) {
	for path, want := range map[string]string{
		"/usr/bin/zip": ZipInfoZIP, `C:\Program Files\7-Zip\7z.exe`: Zip7Zip, "/usr/bin/7za": Zip7Zip,
		"/opt/7zz": Zip7Zip, `C:\Outils\ZIP.EXE`: ZipInfoZIP, "/usr/bin/unzip": "", "/usr/bin/7zip": "",
	} {
		if got := ZipFlavor(path); got != want {
			t.Errorf("%s : %q, attendu %q", path, got, want)
		}
	}
}
