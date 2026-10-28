package prepare

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// fakeDB joue les outils d'export : pg_dump, psql, pg_restore, pg_dumpall, mysqldump,
// mysql, sqlcmd, d'après le nom de l'exécutable.
type fakeDB struct {
	mu        sync.Mutex
	specs     []execx.Spec
	version   string            // réponse à --version
	databases []string          // bases du serveur
	fail      map[string]int    // outil ou base → code de sortie
	truncate  bool              // export MySQL sans sa dernière ligne
	edition   string            // SQL Server
	states    map[string]string // SQL Server : état de chaque base
}

func base(name string) string {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(name)), ".exe")
}

func argValue(args []string, prefix string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	return ""
}

func (f *fakeDB) RunSpec(ctx context.Context, s execx.Spec) (int, error) {
	f.mu.Lock()
	f.specs = append(f.specs, s)
	f.mu.Unlock()
	tool := base(s.Name)
	if slices.Contains(s.Args, "--version") || slices.Contains(s.Args, "-?") {
		io.WriteString(s.Out, f.version)
		return 0, nil
	}
	if code := f.fail[tool]; code != 0 {
		io.WriteString(s.Out, tool+" : échec simulé\n")
		return code, nil
	}
	switch tool {
	case "psql":
		io.WriteString(s.Out, strings.Join(f.databases, "\x00")+"\x00")
	case "pg_dump":
		db := argValue(s.Args, "--dbname=")
		for _, name := range f.databases {
			if strings.Contains(db, "dbname="+pgQuote(name)+" ") && f.fail[name] != 0 {
				return f.fail[name], nil
			}
		}
		data := "PGDMP\x01données de " + db
		if file := argValue(s.Args, "--file="); file != "" {
			return 0, os.WriteFile(file, []byte(data), 0o600)
		}
		io.WriteString(s.Out, data) // export chiffré au fil de l'eau : sortie standard
	case "pg_restore":
	case "pg_dumpall":
		if file := argValue(s.Args, "--file="); file != "" {
			return 0, os.WriteFile(file, []byte("CREATE ROLE kura_backup;\n"), 0o600)
		}
		io.WriteString(s.Out, "CREATE ROLE kura_backup;\n")
	case "mysql", "mariadb":
		for _, n := range f.databases {
			fmt.Fprintf(s.Out, "D:%s\n", strings.ReplaceAll(n, "\n", `\n`))
		}
		io.WriteString(s.Out, "D:mysql\nD:information_schema\nM:"+f.databases[0]+"\n")
	case "mysqldump", "mariadb-dump":
		db := s.Args[len(s.Args)-1]
		fmt.Fprintf(s.Out, "-- MySQL dump\nCREATE DATABASE `%s`;\n", db)
		if !f.truncate {
			io.WriteString(s.Out, "-- Dump completed on 2026-10-28 22:31:02\n")
		}
	case "sqlcmd":
		q := s.Args[len(s.Args)-1]
		switch {
		case strings.Contains(q, "SERVERPROPERTY"):
			fmt.Fprintf(s.Out, "E:%s          \n", f.edition)
			for _, n := range f.databases {
				st := "ONLINE"
				if f.states[n] != "" {
					st = f.states[n]
				}
				u := utf16.Encode([]rune(n))
				b := make([]byte, 2*len(u))
				for i, c := range u {
					b[2*i], b[2*i+1] = byte(c), byte(c>>8)
				}
				fmt.Fprintf(s.Out, "D:%s,%s     \n", strings.ToUpper(hex.EncodeToString(b)), st)
			}
		case strings.HasPrefix(q, "BACKUP DATABASE"):
			_, path, _ := strings.Cut(q, "TO DISK = N'")
			path, _, _ = strings.Cut(path, "' WITH")
			return 0, os.WriteFile(strings.ReplaceAll(path, "''", "'"), []byte("TAPE"), 0o600)
		}
	}
	return 0, nil
}

func (f *fakeDB) calls(tool string) []execx.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []execx.Spec
	for _, s := range f.specs {
		if base(s.Name) == tool && !slices.Contains(s.Args, "--version") && !slices.Contains(s.Args, "-?") {
			out = append(out, s)
		}
	}
	return out
}

// dbFixture : faux dossier d'outils, zone de préparation, configuration.
type dbFixture struct {
	cfg     *config.Config
	staging string
	bin     string
}

func newDBFixture(t *testing.T, engine string, tools ...string) dbFixture {
	t.Helper()
	old := checkToolFile
	checkToolFile = func(p string) error {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("%q : chemin absolu attendu", p)
		}
		return nil
	}
	t.Cleanup(func() { checkToolFile = old })
	dir := t.TempDir()
	f := dbFixture{staging: filepath.Join(dir, "staging"), bin: filepath.Join(dir, "bin")}
	os.MkdirAll(f.staging, 0o700)
	os.MkdirAll(f.bin, 0o755)
	for _, tool := range tools {
		if runtime.GOOS == "windows" {
			tool += ".exe"
		}
		os.WriteFile(filepath.Join(f.bin, tool), nil, 0o755)
	}
	exe := func(n string) string {
		if runtime.GOOS == "windows" {
			n += ".exe"
		}
		return filepath.Join(f.bin, n)
	}
	f.cfg = &config.Config{Tools: config.Tools{PgDump: exe("pg_dump"), MySQLDump: exe(tools[0]), SQLCmd: exe("sqlcmd")}}
	p := config.Prepare{ID: "bases", Type: config.PrepareDBExport, Engine: engine, User: "kura_backup", Password: "credential:bases"}
	if engine == config.EngineSQLServer {
		p.User, p.Password = "", ""
	}
	f.cfg.Prepare = []config.Prepare{p}
	f.cfg.ApplyDefaults()
	return f
}

func (f dbFixture) run(t *testing.T, r Runner, mod func(*Deps)) state.PrepareStatus {
	t.Helper()
	d := Deps{Exec: r, StagingDir: f.staging, Secret: func(ref string) (string, error) {
		if ref != "credential:bases" {
			return "", fmt.Errorf("secret %s inconnu", ref)
		}
		return `mot de passe "secret" \ #1`, nil
	}}
	if mod != nil {
		mod(&d)
	}
	return Run(context.Background(), f.cfg, f.cfg.Prepare[0], d)
}

func (f dbFixture) result(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(f.staging, "bases"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func hasEnv(s execx.Spec, kv string) bool { return slices.Contains(s.Env, kv) }

func TestPostgresExport(t *testing.T) {
	f := newDBFixture(t, config.EnginePostgreSQL, "pg_dump", "psql", "pg_restore", "pg_dumpall")
	db := &fakeDB{version: "pg_dump (PostgreSQL) 17.2", databases: []string{"compta", "ventes"}}
	ps := f.run(t, db, nil)
	if ps.Result != state.ResultOK || ps.Files != 2 || !slices.Equal(ps.Databases, []string{"compta", "ventes"}) || ps.Size == 0 {
		t.Fatalf("%+v", ps)
	}
	if got := f.result(t); !slices.Equal(got, []string{"compta.dump", "globals.sql", "ventes.dump"}) {
		t.Fatalf("fichiers : %v", got)
	}
	dumps := db.calls("pg_dump")
	if len(dumps) != 2 || len(db.calls("pg_restore")) != 2 || len(db.calls("psql")) != 1 {
		t.Fatalf("appels : %d pg_dump, %d pg_restore", len(dumps), len(db.calls("pg_restore")))
	}
	for _, s := range append(dumps, db.calls("psql")...) {
		if !hasEnv(s, `PGPASSWORD=mot de passe "secret" \ #1`) {
			t.Errorf("mot de passe absent de l'environnement de %s", s.Name)
		}
		if strings.Contains(strings.Join(s.Args, " "), "secret") {
			t.Errorf("mot de passe en argument : %v", s.Args)
		}
		if s.Dir != f.staging {
			t.Errorf("dossier de travail %q", s.Dir)
		}
	}
	conn := argValue(dumps[0].Args, "--dbname=")
	if conn != "dbname='compta' host='localhost' port=5432 user='kura_backup' application_name=kura-agent connect_timeout=30" {
		t.Errorf("connexion : %s", conn)
	}
	if !slices.Contains(dumps[0].Args, "--format=custom") || !slices.Contains(dumps[0].Args, "--no-password") {
		t.Errorf("options : %v", dumps[0].Args)
	}
	if left, _ := os.ReadDir(f.staging); len(left) != 1 {
		t.Errorf("zone : %v", left)
	}
}

// Une base dont le nom contient « = » ou une apostrophe ne devient jamais une chaîne de
// connexion : elle reste une valeur entre apostrophes.
func TestPostgresConninfoQuoting(t *testing.T) {
	e := &pgEngine{dbRun: &dbRun{p: config.Prepare{Socket: "/run/postgresql"}}}
	got := e.conninfo(`x' host=evil.example`)
	if got != `dbname='x\' host=evil.example' host='/run/postgresql' application_name=kura-agent connect_timeout=30` {
		t.Fatalf("%s", got)
	}
}

func TestPostgresFailures(t *testing.T) {
	for _, c := range []struct {
		name    string
		fail    map[string]int
		version string
		result  string
		msg     string
		files   []string
	}{
		{"une base en échec", map[string]int{"ventes": 1}, "", state.ResultError, "1 base(s) sur 2", nil},
		{"relecture refusée", map[string]int{"pg_restore": 1}, "", state.ResultError, "pg_restore", nil},
		{"connexion refusée", map[string]int{"psql": 2}, "", state.ResultError, "liste des bases", nil},
		{"rôles non exportés", map[string]int{"pg_dumpall": 1}, "", state.ResultWarning, "rôles non exportés",
			[]string{"compta.dump", "ventes.dump"}},
		{"version ancienne", nil, "pg_dump (PostgreSQL) 9.6.24", state.ResultError, "trop ancienne", nil},
		{"autre outil", nil, "zip 3.0", state.ResultError, "pas pg_dump", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDBFixture(t, config.EnginePostgreSQL, "pg_dump", "psql", "pg_restore", "pg_dumpall")
			db := &fakeDB{fail: c.fail, version: c.version, databases: []string{"compta", "ventes"}}
			if db.version == "" {
				db.version = "pg_dump (PostgreSQL) 16.4"
			}
			ps := f.run(t, db, nil)
			if ps.Result != c.result || !strings.Contains(ps.Message, c.msg) {
				t.Fatalf("%+v", ps)
			}
			if got := f.result(t); !slices.Equal(got, c.files) {
				t.Errorf("résultat : %v", got)
			}
		})
	}
}

func TestPostgresMissingClientTools(t *testing.T) {
	f := newDBFixture(t, config.EnginePostgreSQL, "pg_dump", "psql")
	ps := f.run(t, &fakeDB{version: "pg_dump (PostgreSQL) 17.2"}, nil)
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "pg_restore absent") {
		t.Fatalf("%+v", ps)
	}
}

func TestSecretMissing(t *testing.T) {
	f := newDBFixture(t, config.EnginePostgreSQL, "pg_dump", "psql", "pg_restore")
	db := &fakeDB{version: "pg_dump (PostgreSQL) 17.2", databases: []string{"compta"}}
	ps := f.run(t, db, func(d *Deps) {
		d.Secret = func(string) (string, error) { return "", fmt.Errorf("secret introuvable") }
	})
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "credentials set bases") || len(db.specs) != 0 {
		t.Fatalf("%+v (%d appels)", ps, len(db.specs))
	}
}

// Le dernier export réussi, plus 20 % et la réserve : sinon rien n'est lancé.
func TestDBExportNeedsSpaceForLastSize(t *testing.T) {
	f := newDBFixture(t, config.EnginePostgreSQL, "pg_dump", "psql", "pg_restore", "pg_dumpall")
	db := &fakeDB{version: "pg_dump (PostgreSQL) 17.2", databases: []string{"compta"}}
	ps := f.run(t, db, func(d *Deps) {
		d.LastSize = 10 << 30
		d.FreeSpace = func(string) (uint64, error) { return 13 << 30, nil }
	})
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "14,0 Go demandés") || len(db.specs) != 0 {
		t.Fatalf("%+v", ps)
	}
	ps = f.run(t, db, func(d *Deps) {
		d.LastSize = 10 << 30
		d.FreeSpace = func(string) (uint64, error) { return 15 << 30, nil }
	})
	if ps.Result != state.ResultOK {
		t.Fatalf("%+v", ps)
	}
}

func TestMySQLExport(t *testing.T) {
	for _, c := range []struct{ tool, version, client string }{
		{"mysqldump", "mysqldump  Ver 8.4.3 for Linux on x86_64 (MySQL Community Server - GPL)", "mysql"},
		{"mariadb-dump", "mariadb-dump from 11.4.4-MariaDB, client 10.19 for debian-linux-gnu (x86_64)", "mariadb"},
	} {
		t.Run(c.tool, func(t *testing.T) {
			f := newDBFixture(t, config.EngineMySQL, c.tool, c.client)
			var options string
			db := &fakeDB{version: c.version, databases: []string{"site", "crm"}}
			ps := f.run(t, readOptions{db, &options}, nil)
			if ps.Result != state.ResultOK || ps.Files != 2 {
				t.Fatalf("%+v", ps)
			}
			if got := f.result(t); !slices.Equal(got, []string{"crm.sql.gz", "site.sql.gz"}) {
				t.Fatalf("fichiers : %v", got)
			}
			gz, err := os.Open(filepath.Join(f.staging, "bases", "site.sql.gz"))
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			zr, err := gzip.NewReader(gz)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(zr)
			if !strings.Contains(string(data), "CREATE DATABASE `site`") {
				t.Fatalf("export : %q", data)
			}
			dumps := db.calls(c.tool)
			args := dumps[len(dumps)-1].Args
			if !strings.HasPrefix(args[0], "--defaults-extra-file=") || !slices.Contains(args, "--single-transaction") ||
				!slices.Equal(args[len(args)-2:], []string{"--databases", "crm"}) {
				t.Fatalf("options : %v", args)
			}
			if stats := slices.Contains(args, "--column-statistics=0"); stats != (c.tool == "mysqldump") {
				t.Errorf("--column-statistics=0 : %v", stats)
			}
			for _, s := range db.specs {
				if strings.Contains(strings.Join(append(s.Args, s.Env...), " "), "secret") {
					t.Errorf("mot de passe visible : %v %v", s.Args, s.Env)
				}
			}
			want := "[client]\nhost=\"localhost\"\nport=3306\nuser=\"kura_backup\"\npassword=\"mot de passe \"secret\" \\\\ #1\"\n"
			if options != want {
				t.Errorf("fichier d'options :\n%s\nattendu :\n%s", options, want)
			}
			if _, err := os.Stat(filepath.Join(f.staging, "bases.cnf")); !os.IsNotExist(err) {
				t.Error("fichier d'options resté dans la zone")
			}
		})
	}
}

// readOptions lit le fichier d'options au moment où l'outil le reçoit.
type readOptions struct {
	*fakeDB
	got *string
}

func (r readOptions) RunSpec(ctx context.Context, s execx.Spec) (int, error) {
	if len(s.Args) > 0 {
		if p, ok := strings.CutPrefix(s.Args[0], "--defaults-extra-file="); ok {
			data, _ := os.ReadFile(p)
			*r.got = string(data)
		}
	}
	return r.fakeDB.RunSpec(ctx, s)
}

func TestMySQLTruncatedExport(t *testing.T) {
	f := newDBFixture(t, config.EngineMySQL, "mysqldump", "mysql")
	ps := f.run(t, &fakeDB{version: "mysqldump  Ver 8.0.39 for Linux", databases: []string{"site"}, truncate: true}, nil)
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "Dump completed") {
		t.Fatalf("%+v", ps)
	}
	if got := f.result(t); len(got) != 0 {
		t.Fatalf("export incomplet gardé : %v", got)
	}
}

func TestMySQLUnescape(t *testing.T) {
	if got := myUnescape(`a\nb\tc\\d\0`); got != "a\nb\tc\\d\x00" {
		t.Fatalf("%q", got)
	}
}

func TestSQLServerExport(t *testing.T) {
	f := newDBFixture(t, config.EngineSQLServer, "sqlcmd")
	f.cfg.Prepare[0].Instance = `localhost\SQLEXPRESS`
	f.cfg.Prepare[0].Verify = true
	granted, revoked := "", 0
	old := grantSQLService
	grantSQLService = func(instance, dir string) (func() error, error) {
		granted = instance + " " + dir
		return func() error { revoked++; return nil }, nil
	}
	t.Cleanup(func() { grantSQLService = old })
	db := &fakeDB{version: "Version 15.0.4298.1 NT", edition: "Express Edition (64-bit)",
		databases: []string{"Gestion", "L'Atelier]"}, states: map[string]string{}}
	ps := f.run(t, db, nil)
	if ps.Result != state.ResultOK || ps.Files != 2 {
		t.Fatalf("%+v", ps)
	}
	if got := f.result(t); !slices.Equal(got, []string{"Gestion.bak", "L%27Atelier%5D.bak"}) {
		t.Fatalf("fichiers : %v", got)
	}
	if !strings.HasPrefix(granted, `localhost\SQLEXPRESS `) || !strings.HasSuffix(granted, "bases.tmp") || revoked != 1 {
		t.Fatalf("droits du service : %q, rendus %d fois", granted, revoked)
	}
	var backups, verifies []string
	for _, s := range db.calls("sqlcmd") {
		q := s.Args[len(s.Args)-1]
		switch {
		case strings.HasPrefix(q, "BACKUP"):
			backups = append(backups, q)
		case strings.HasPrefix(q, "RESTORE VERIFYONLY"):
			verifies = append(verifies, q)
		}
		for _, opt := range []string{"-b", "-x", "-E"} {
			if !slices.Contains(s.Args, opt) {
				t.Errorf("option %s absente : %v", opt, s.Args)
			}
		}
	}
	if len(backups) != 2 || len(verifies) != 2 {
		t.Fatalf("requêtes : %v %v", backups, verifies)
	}
	if !strings.HasPrefix(backups[1], "BACKUP DATABASE [L'Atelier]]] TO DISK = N'") || strings.Contains(backups[1], "COMPRESSION") ||
		!strings.HasSuffix(backups[1], "L%27Atelier%5D.bak' WITH COPY_ONLY, CHECKSUM, INIT;") {
		t.Fatalf("sauvegarde : %s", backups[1])
	}
}

func TestSQLServerOfflineAndCompression(t *testing.T) {
	f := newDBFixture(t, config.EngineSQLServer, "sqlcmd")
	old := grantSQLService
	grantSQLService = func(string, string) (func() error, error) { return func() error { return nil }, nil }
	t.Cleanup(func() { grantSQLService = old })
	db := &fakeDB{version: "sqlcmd: 1.8.2", edition: "Standard Edition (64-bit)",
		databases: []string{"Gestion", "Archives"}, states: map[string]string{"Archives": "OFFLINE"}}
	ps := f.run(t, db, nil)
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "OFFLINE") {
		t.Fatalf("%+v", ps)
	}
	for _, s := range db.calls("sqlcmd") {
		if q := s.Args[len(s.Args)-1]; strings.HasPrefix(q, "BACKUP") && !strings.Contains(q, "COMPRESSION") {
			t.Errorf("édition Standard sans compression : %s", q)
		}
	}
}

func TestDBFileNames(t *testing.T) {
	for name, want := range map[string]string{
		"compta": "compta", "Comptabilité 2026": "Comptabilité 2026", "a/b": "a%2Fb", ".cache": "%2Ecache",
		"fin.": "fin%2E", "con": "%63on", "COM1": "%43OM1", "x:y*z": "x%3Ay%2Az", "100%": "100%25",
	} {
		if got := dbFileName(name); got != want {
			t.Errorf("%q → %q, attendu %q", name, got, want)
		}
	}
	if _, err := exportFiles([]string{"Compta", "compta"}, ".dump"); err == nil {
		t.Error("deux bases au même fichier acceptées")
	}
}
