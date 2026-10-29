package config

import (
	"strings"
	"testing"
	"time"
)

const withDocker = `
version: 1
profile:
  endpoint: https://s3.eu-central-1.wasabisys.com
  bucket: acme-srv01
schedule:
  type: daily
  time: "22:30"
prepare:
  - id: appli
    type: docker_volume
    volumes: [appli_db, appli_uploads]
    exclude: ["*/cache/*"]
  - id: appli-chiffre
    type: age
    input: appli
    recipients:
      - age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p
  - id: statique
    type: docker_volume
    volumes: [certificats]
    pause: false
jobs:
  - id: appli
    source: "${STAGING:appli-chiffre}"
    destination: DOCKER/APPLI
  - id: statique
    source: "${STAGING:statique}"
    destination: DOCKER/STATIQUE
`

func TestDockerVolumeLoads(t *testing.T) {
	c, err := Parse([]byte(withDocker), LoadOptions{Env: testEnv()})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := c.PrepareByID("appli")
	if !a.Paused() || a.Socket != DockerSocket || a.DataRoot != "" || a.MaxPause.Duration != 15*time.Minute || len(a.Volumes) != 2 {
		t.Fatalf("valeurs par défaut avec gel : %+v", a)
	}
	s, _ := c.PrepareByID("statique")
	if s.Paused() || s.Socket != "" || s.DataRoot != DockerDataRoot {
		t.Fatalf("valeurs par défaut sans gel : %+v", s)
	}
}

func TestDockerVolumeValidation(t *testing.T) {
	cases := []struct{ name, from, to, field string }{
		{"sans volume", "volumes: [appli_db, appli_uploads]", "volumes: []", "prepare[0].volumes"},
		{"nom invalide", "volumes: [appli_db, appli_uploads]", "volumes: [appli_db, \"../etc\"]", "prepare[0].volumes[1]"},
		{"doublon", "volumes: [appli_db, appli_uploads]", "volumes: [appli_db, appli_db]", "déjà listé"},
		{"deux préparations", "volumes: [certificats]", "volumes: [appli_db]", "déjà archivé par la préparation appli"},
		{"socket sans gel", "    pause: false\n", "    pause: false\n    socket: /var/run/docker.sock\n", "prepare[2].socket"},
		{"data_root avec gel", "exclude: [\"*/cache/*\"]", "exclude: [\"*/cache/*\"]\n    data_root: /srv/docker", "prepare[0].data_root"},
		{"socket relatif", "exclude: [\"*/cache/*\"]", "exclude: [\"*/cache/*\"]\n    socket: docker.sock", "prepare[0].socket"},
		{"max_pause au-delà", "exclude: [\"*/cache/*\"]", "exclude: [\"*/cache/*\"]\n    timeout: 10m\n    max_pause: 20m", "prepare[0].max_pause"},
		{"clé d'un autre type", "exclude: [\"*/cache/*\"]", "exclude: [\"*/cache/*\"]\n    source: /srv", "prepare[0]"},
		{"pré-tâche", "exclude: [\"*/cache/*\"]", "exclude: [\"*/cache/*\"]\n    depends_on_pre_task: outlook.exe", "prepare[0].depends_on_pre_task"},
		{"volumes ailleurs", "    input: appli\n", "    input: appli\n    volumes: [x]\n", "réservés à docker_volume"},
		{"clair envoyé", "${STAGING:appli-chiffre}", "${STAGING:appli}", "son clair ne part jamais"},
	}
	for _, c := range cases {
		yaml := strings.Replace(withDocker, c.from, c.to, 1)
		if yaml == withDocker {
			t.Fatalf("%s : %q introuvable", c.name, c.from)
		}
		if err := parseWith(t, yaml, LoadOptions{}); err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s : erreur sur %s attendue, reçu %v", c.name, c.field, err)
		}
	}
}
