package config

import (
	"slices"
	"strings"
	"testing"
)

const withDBExport = `
version: 1
profile:
  endpoint: https://s3.eu-central-1.wasabisys.com
  bucket: acme-srv01
schedule:
  type: daily
  time: "22:30"
tools:
  pg_dump: "C:/Program Files/PostgreSQL/17/bin/pg_dump.exe"
  mysqldump: "C:/Program Files/MariaDB 11.4/bin/mariadb-dump.exe"
  sqlcmd: "C:/Program Files/SqlCmd/sqlcmd.exe"
prepare:
  - id: compta-db
    type: db_export
    engine: postgresql
    user: kura_backup
    password: credential:compta-db
    databases: [compta]
  - id: site-db
    type: db_export
    engine: mysql
    host: db.example.lan
    port: 3307
    user: kura_backup
    password: credential:site-db
  - id: gestion-db
    type: db_export
    engine: sqlserver
    instance: 'localhost\SQLEXPRESS'
    databases: [Gestion, "L'Atelier"]
    verify: true
jobs:
  - id: compta
    source: "${STAGING:compta-db}"
    destination: BASES/COMPTA
  - id: site
    source: "${STAGING:site-db}"
    destination: BASES/SITE
  - id: gestion
    source: "${STAGING:gestion-db}"
    destination: BASES/GESTION
`

func TestDBExportLoads(t *testing.T) {
	c, err := Parse([]byte(withDBExport), LoadOptions{Env: testEnv()})
	if err != nil {
		t.Fatal(err)
	}
	pg, _ := c.PrepareByID("compta-db")
	if pg.Host != "localhost" || pg.Port != 5432 || pg.Instance != "" {
		t.Errorf("valeurs par défaut PostgreSQL : %+v", pg)
	}
	my, _ := c.PrepareByID("site-db")
	if my.Host != "db.example.lan" || my.Port != 3307 {
		t.Errorf("MySQL : %+v", my)
	}
	ms, _ := c.PrepareByID("gestion-db")
	if ms.Instance != `localhost\SQLEXPRESS` || ms.Host != "" || ms.Port != 0 || !ms.Verify {
		t.Errorf("SQL Server : %+v", ms)
	}
	var refs []string
	for _, r := range c.CredentialRefs() {
		refs = append(refs, r.Field+"="+r.Name)
	}
	for _, want := range []string{"prepare[0].password=compta-db", "prepare[1].password=site-db"} {
		if !slices.Contains(refs, want) {
			t.Errorf("secret %s absent de CredentialRefs : %v", want, refs)
		}
	}
}

// Une connexion par socket, sans utilisateur ni mot de passe (authentification peer ou
// unix_socket), sans valeurs par défaut de host et port.
func TestDBExportSocket(t *testing.T) {
	yaml := strings.Replace(withDBExport, `    user: kura_backup
    password: credential:compta-db
`, "    socket: /run/postgresql\n", 1)
	yaml = strings.Replace(yaml, `"C:/Program Files/PostgreSQL/17/bin/pg_dump.exe"`, "/usr/lib/postgresql/16/bin/pg_dump", 1)
	c, err := Parse([]byte(yaml), LoadOptions{Env: testEnv()})
	if err != nil {
		t.Fatal(err)
	}
	if pg, _ := c.PrepareByID("compta-db"); pg.Host != "" || pg.Port != 0 || pg.Socket != "/run/postgresql" {
		t.Fatalf("%+v", pg)
	}
}

func TestDBExportValidation(t *testing.T) {
	cases := []struct {
		name, from, to, field string
	}{
		{"moteur absent", "    engine: postgresql\n", "", "prepare[0].engine"},
		{"moteur inconnu", "engine: postgresql", "engine: oracle", "prepare[0].engine"},
		{"outil absent", "  pg_dump: \"C:/Program Files/PostgreSQL/17/bin/pg_dump.exe\"\n", "", "prepare[0].engine"},
		{"outil relatif", "C:/Program Files/PostgreSQL/17/bin/pg_dump.exe", "pg_dump", "tools.pg_dump"},
		{"mot de passe en clair", "password: credential:compta-db", "password: S3cret!", "prepare[0].password"},
		{"utilisateur absent", "    user: kura_backup\n    password: credential:compta-db\n", "", "prepare[0].user"},
		{"mot de passe sans utilisateur", "    user: kura_backup\n    password: credential:compta-db\n", "    password: credential:compta-db\n", "prepare[0].user"},
		{"utilisateur option", "user: kura_backup\n    password: credential:compta-db", "user: -kura\n    password: credential:compta-db", "prepare[0].user"},
		{"base option", "databases: [compta]", "databases: [\"--all\"]", "prepare[0].databases[0]"},
		{"base en double", "databases: [compta]", "databases: [compta, Compta]", "prepare[0].databases[1]"},
		{"hôte option", "host: db.example.lan", "host: \"-oProxyCommand\"", "prepare[1].host"},
		{"port", "port: 3307", "port: 70000", "prepare[1].port"},
		{"socket et hôte", "port: 3307", "port: 3307\n    socket: /run/mysqld/mysqld.sock", "prepare[1].socket"},
		{"socket relatif", "host: db.example.lan\n    port: 3307", "socket: mysqld.sock", "prepare[1].socket"},
		{"instance distante", `instance: 'localhost\SQLEXPRESS'`, `instance: 'srv-sql\SQLEXPRESS'`, "prepare[2].instance"},
		{"hôte pour SQL Server", `instance: 'localhost\SQLEXPRESS'`, "host: localhost", "prepare[2]"},
		{"instance hors SQL Server", "port: 3307", "port: 3307\n    instance: localhost", "prepare[1].instance"},
		{"verify hors SQL Server", "port: 3307", "port: 3307\n    verify: true", "prepare[1].verify"},
		{"source pour un export", "engine: postgresql", "engine: postgresql\n    source: D:/Compta", "prepare[0]"},
	}
	for _, c := range cases {
		yaml := strings.Replace(withDBExport, c.from, c.to, 1)
		if yaml == withDBExport {
			t.Fatalf("%s : %q introuvable dans le modèle", c.name, c.from)
		}
		err := parseWith(t, yaml, LoadOptions{})
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s : erreur sur %s attendue, reçu %v", c.name, c.field, err)
		}
	}
	for _, inst := range []string{"localhost", ".", `(local)\COMPTA`, "localhost,1433", `127.0.0.1\SQLEXPRESS`} {
		yaml := strings.Replace(withDBExport, `'localhost\SQLEXPRESS'`, "'"+inst+"'", 1)
		if err := parseWith(t, yaml, LoadOptions{}); err != nil {
			t.Errorf("instance %q refusée : %v", inst, err)
		}
	}
}

// Une préparation zip ne prend pas les clés d'un export.
func TestZipRefusesDBKeys(t *testing.T) {
	yaml := strings.Replace(withPrepare, "exclude:", "engine: postgresql\n    exclude:", 1)
	if err := parseWith(t, yaml, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "réservés à db_export") {
		t.Fatalf("%v", err)
	}
}
