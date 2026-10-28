package prepare

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Essais contre de vraies bases, avec les vrais outils, lancés par la CI (services
// PostgreSQL, MySQL, MariaDB sous Linux ; SQL Server Express sous Windows). Sans leurs
// variables d'environnement, ils sont sautés, sauf si KURA_TEST_DB=1 les exige.

func needEnv(t *testing.T, keys ...string) []string {
	t.Helper()
	var vals []string
	for _, k := range keys {
		v := os.Getenv(k)
		if v == "" {
			if os.Getenv("KURA_TEST_DB") == "1" {
				t.Fatalf("%s absent alors que KURA_TEST_DB=1", k)
			}
			t.Skipf("%s absent : essai contre une vraie base sauté", k)
		}
		vals = append(vals, v)
	}
	return vals
}

// admin lance un outil client avec les droits d'administration de la base de test.
func admin(t *testing.T, env []string, stdin io.Reader, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s : %v\n%s%s", filepath.Base(name), strings.Join(args, " "), err, out, stderr.String())
	}
	return string(out)
}

func realRun(t *testing.T, cfg *config.Config, secret string) (state.PrepareStatus, string, string) {
	t.Helper()
	cfg.ApplyDefaults()
	staging := filepath.Join(t.TempDir(), "staging")
	os.MkdirAll(staging, 0o700)
	var log bytes.Buffer
	ps := Run(context.Background(), cfg, cfg.Prepare[0], Deps{
		Exec: execx.System{}, StagingDir: staging, Out: &log,
		Secret: func(string) (string, error) { return secret, nil },
	})
	return ps, filepath.Join(staging, cfg.Prepare[0].ID), log.String()
}

func TestPostgresRealExport(t *testing.T) {
	v := needEnv(t, "KURA_TEST_PG_DUMP", "KURA_TEST_PG_HOST", "KURA_TEST_PG_PORT", "KURA_TEST_PG_PASSWORD")
	dump, host, port, adminPW := v[0], v[1], v[2], v[3]
	psql, restore := sibling(dump, "psql"), sibling(dump, "pg_restore")
	env := []string{"PGPASSWORD=" + adminPW}
	conn := func(db string) string {
		return "dbname=" + pgQuote(db) + " host=" + host + " port=" + port + " user=postgres"
	}
	sql := func(db, q string) string {
		return admin(t, env, nil, psql, "-X", "-A", "-t", "-q", "-v", "ON_ERROR_STOP=1", "-d", conn(db), "-c", q)
	}
	weird := "L'atelier = 2"
	for _, db := range []string{"compta", weird, "compta_copie"} {
		sql("postgres", "DROP DATABASE IF EXISTS "+pgIdent(db))
	}
	sql("postgres", "DROP ROLE IF EXISTS kura_backup")
	sql("postgres", "CREATE ROLE kura_backup LOGIN PASSWORD 'kura-test'; GRANT pg_read_all_data TO kura_backup")
	sql("postgres", "CREATE DATABASE compta")
	sql("postgres", "CREATE DATABASE "+pgIdent(weird))
	sql("compta", "CREATE TABLE ecritures(id int PRIMARY KEY, libelle text); "+
		"INSERT INTO ecritures SELECT g, 'ligne ' || g FROM generate_series(1, 1000) g")

	p, _ := strconv.Atoi(port)
	cfg := &config.Config{Tools: config.Tools{PgDump: dump}, Prepare: []config.Prepare{{ID: "bases", Type: config.PrepareDBExport,
		Engine: config.EnginePostgreSQL, Host: host, Port: p, User: "kura_backup", Password: "credential:pg"}}}
	ps, dir, log := realRun(t, cfg, "kura-test")
	if ps.Result != state.ResultOK || !slices.Contains(ps.Databases, "compta") || !slices.Contains(ps.Databases, weird) {
		t.Fatalf("%+v\n%s", ps, log)
	}
	for _, f := range []string{"compta.dump", "L%27atelier %3D 2.dump", "globals.sql"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s : %v", f, err)
		}
	}
	// Export rechargé dans une autre base, puis comparé.
	sql("postgres", "CREATE DATABASE compta_copie")
	admin(t, env, nil, restore, "--no-owner", "--exit-on-error", "--dbname="+conn("compta_copie"), filepath.Join(dir, "compta.dump"))
	if n := strings.TrimSpace(sql("compta_copie", "SELECT count(*) FROM ecritures")); n != "1000" {
		t.Fatalf("export rechargé : %s lignes", n)
	}

	// Export chiffré par age au fil de l'eau, déchiffré puis rechargé.
	id := newAgeIdentity(t)
	cfg.Prepare = append(cfg.Prepare, config.Prepare{ID: "bases-chiffre", Type: config.PrepareAge, Input: "bases",
		Recipients: []config.Recipient{{Age: id.Recipient().String()}}})
	cfg.ApplyDefaults()
	staging := filepath.Join(t.TempDir(), "staging")
	os.MkdirAll(staging, 0o700)
	ps = Run(context.Background(), cfg, cfg.Prepare[1], Deps{Exec: execx.System{}, StagingDir: staging,
		Secret: func(string) (string, error) { return "kura-test", nil }})
	if ps.Result != state.ResultOK {
		t.Fatalf("export chiffré : %+v", ps)
	}
	clear := filepath.Join(t.TempDir(), "compta.dump")
	os.WriteFile(clear, []byte(decryptAge(t, filepath.Join(staging, "bases-chiffre", "compta.dump.age"), id)), 0o600)
	sql("postgres", "DROP DATABASE IF EXISTS compta_copie")
	sql("postgres", "CREATE DATABASE compta_copie")
	admin(t, env, nil, restore, "--no-owner", "--exit-on-error", "--dbname="+conn("compta_copie"), clear)
	if n := strings.TrimSpace(sql("compta_copie", "SELECT count(*) FROM ecritures")); n != "1000" {
		t.Fatalf("export chiffré rechargé : %s lignes", n)
	}
	cfg.Prepare = cfg.Prepare[:1]

	// Mauvais mot de passe : échec, rien de gardé.
	ps, dir, _ = realRun(t, cfg, "faux")
	if ps.Result != state.ResultError {
		t.Fatalf("mauvais mot de passe accepté : %+v", ps)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("résultat gardé après un échec")
	}
}

// pgIdent : identifiant PostgreSQL entre guillemets.
func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// TestMySQLRealExport : KURA_TEST_MYSQL_TARGETS liste les serveurs (hôte:port, séparés
// par des virgules) que l'outil de KURA_TEST_MYSQLDUMP exporte : MySQL avec mysqldump,
// MariaDB avec mariadb-dump.
func TestMySQLRealExport(t *testing.T) {
	v := needEnv(t, "KURA_TEST_MYSQLDUMP", "KURA_TEST_MYSQL_TARGETS", "KURA_TEST_MYSQL_ROOT_PASSWORD")
	dump, targets, rootPW := v[0], v[1], v[2]
	client := sibling(dump, "mysql", "mariadb")
	for _, target := range strings.Split(targets, ",") {
		t.Run(target, func(t *testing.T) {
			host, port, _ := strings.Cut(target, ":")
			env := []string{"MYSQL_PWD=" + rootPW}
			sql := func(q string) string {
				return admin(t, env, nil, client, "-h", host, "-P", port, "-u", "root", "--default-character-set=utf8mb4",
					"--batch", "--skip-column-names", "-e", q)
			}
			sql("DROP DATABASE IF EXISTS site; DROP USER IF EXISTS 'kura_backup'@'%'; " +
				"CREATE USER 'kura_backup'@'%' IDENTIFIED BY 'kura-test'; " +
				"GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES, PROCESS ON *.* TO 'kura_backup'@'%'; " +
				"CREATE DATABASE site; " +
				"CREATE TABLE site.pages(id int PRIMARY KEY, titre varchar(100)) ENGINE=InnoDB; " +
				"CREATE TABLE site.journal(id int PRIMARY KEY) ENGINE=MyISAM; " +
				"INSERT INTO site.pages VALUES (1, 'Accueil'), (2, 'L''équipe'), (3, 'Contact')")
			p, _ := strconv.Atoi(port)
			cfg := &config.Config{Tools: config.Tools{MySQLDump: dump}, Prepare: []config.Prepare{{ID: "bases", Type: config.PrepareDBExport,
				Engine: config.EngineMySQL, Host: host, Port: p, User: "kura_backup", Password: "credential:my", Databases: []string{"site"}}}}
			ps, dir, log := realRun(t, cfg, "kura-test")
			if ps.Result != state.ResultOK || ps.Files != 1 {
				t.Fatalf("%+v\n%s", ps, log)
			}
			f, err := os.Open(filepath.Join(dir, "site.sql.gz"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			zr, err := gzip.NewReader(f)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			// Export rechargé à la place de la base, puis comparé.
			sql("DROP DATABASE site")
			admin(t, env, bytes.NewReader(data), client, "-h", host, "-P", port, "-u", "root", "--default-character-set=utf8mb4")
			if n := strings.TrimSpace(sql("SELECT titre FROM site.pages WHERE id = 2")); n != "L'équipe" {
				t.Fatalf("export rechargé : %q", n)
			}
		})
	}
}

func TestSQLServerRealExport(t *testing.T) {
	v := needEnv(t, "KURA_TEST_SQLCMD", "KURA_TEST_SQL_INSTANCE")
	sqlcmd, instance := v[0], v[1]
	sql := func(q string) string {
		return admin(t, nil, nil, sqlcmd, "-S", instance, "-E", "-b", "-C", "-x", "-h", "-1", "-Q", "SET NOCOUNT ON; "+q)
	}
	weird := "L'Atelier]"
	for _, db := range []string{"Gestion", weird} {
		sql("IF DB_ID(" + msString(db) + ") IS NOT NULL DROP DATABASE " + msIdent(db) + "; CREATE DATABASE " + msIdent(db) + ";")
	}
	sql("CREATE TABLE Gestion.dbo.Factures(id int PRIMARY KEY, client nvarchar(100)); " +
		"INSERT INTO Gestion.dbo.Factures VALUES (1, N'Dupont'), (2, N'Martin');")
	cfg := &config.Config{Tools: config.Tools{SQLCmd: sqlcmd}, Prepare: []config.Prepare{{ID: "bases", Type: config.PrepareDBExport,
		Engine: config.EngineSQLServer, Instance: instance, Databases: []string{"Gestion", weird}, Verify: true}}}
	ps, dir, log := realRun(t, cfg, "")
	if ps.Result != state.ResultOK || ps.Files != 2 {
		t.Fatalf("%+v\n%s", ps, log)
	}
	for _, f := range []string{"Gestion.bak", "L%27Atelier%5D.bak"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Size() == 0 {
			t.Fatalf("%s : %v", f, err)
		}
	}
	// Le service n'a plus aucun droit sur les sauvegardes ni sur le dossier : rendus à
	// l'agent (propriétaire Administrateurs).
	checkReclaimed(t, dir, msServiceName(instance))
}
