package prepare

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// pgEngine : PostgreSQL, par pg_dump (format custom, compressé), psql (liste des bases),
// pg_restore (relecture complète de chaque export) et pg_dumpall (rôles), tous pris
// dans le dossier de tools.pg_dump.
type pgEngine struct {
	*dbRun
	dump, psql, restore, dumpall string
}

// pgMin : pg_dump 12 au moins ; le serveur, lui, refuse un pg_dump plus ancien que lui.
const pgMin = 12

var pgVersion = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)(?:\.(\d+))?`)

func (e *pgEngine) check(ctx context.Context) (string, error) {
	if err := checkToolFile(e.dump); err != nil {
		return "", fmt.Errorf("tools.pg_dump : %w", err)
	}
	e.psql, e.restore = sibling(e.dump, "psql"), sibling(e.dump, "pg_restore")
	for name, p := range map[string]string{"psql": e.psql, "pg_restore": e.restore} {
		if p == "" {
			return "", fmt.Errorf("tools.pg_dump : %s absent du dossier de pg_dump (%s) ; installer les outils clients complets de PostgreSQL", name, filepath.Dir(e.dump))
		}
		if err := checkToolFile(p); err != nil {
			return "", fmt.Errorf("tools.pg_dump : %w", err)
		}
	}
	// pg_dumpall (rôles) est facultatif : sans lui, un avertissement.
	if e.dumpall = sibling(e.dump, "pg_dumpall"); e.dumpall != "" && checkToolFile(e.dumpall) != nil {
		e.dumpall = ""
	}
	out, err := toolVersion(ctx, e.d.Exec, e.dump, "--version")
	if err != nil {
		return "", fmt.Errorf("tools.pg_dump : %w", err)
	}
	v, found, ok := versionAtLeast(out, pgVersion, pgMin, 0)
	switch {
	case !found:
		return "", fmt.Errorf("tools.pg_dump : %s n'est pas pg_dump de PostgreSQL (version illisible : %q)", e.dump, firstLine(out))
	case !ok:
		return v, fmt.Errorf("tools.pg_dump : version %s trop ancienne (%d ou plus récente, et au moins celle du serveur)", v, pgMin)
	}
	return v, nil
}

func (e *pgEngine) open() error { return nil }
func (e *pgEngine) close()      {}
func (e *pgEngine) ext() string { return ".dump" }

// conninfo : chaîne de connexion libpq. Le nom de la base y est toujours une valeur
// entre apostrophes : passé seul, un nom contenant « = » serait lu comme une chaîne de
// connexion (et pourrait désigner un autre serveur, qui recevrait le mot de passe).
func (e *pgEngine) conninfo(db string) string {
	kv := []string{"dbname=" + pgQuote(db)}
	if e.p.Socket != "" {
		kv = append(kv, "host="+pgQuote(e.p.Socket))
	} else {
		kv = append(kv, "host="+pgQuote(e.p.Host), "port="+strconv.Itoa(e.p.Port))
	}
	if e.p.User != "" {
		kv = append(kv, "user="+pgQuote(e.p.User))
	}
	return strings.Join(append(kv, "application_name=kura-agent", "connect_timeout=30"), " ")
}

// pgQuote : valeur libpq entre apostrophes, « \ » et « ' » échappés.
func pgQuote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// env : mot de passe dans la seule variable du processus lancé, jamais en argument.
func (e *pgEngine) env() []string {
	if e.password == "" {
		return nil
	}
	return []string{"PGPASSWORD=" + e.password}
}

func (e *pgEngine) list(ctx context.Context) ([]string, error) {
	// Noms séparés par un octet nul : un nom de base peut contenir un retour à la ligne.
	args := []string{"-X", "-A", "-t", "-q", "--record-separator-zero", "-v", "ON_ERROR_STOP=1", "-d", e.conninfo("postgres"),
		"-c", "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname"}
	out, code, err := e.capture(ctx, e.toolSpec(e.psql, args, e.env()...))
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("psql a échoué (code %d) : connexion ou droits ; voir le journal de la préparation", code)
	}
	var names []string
	for _, n := range strings.Split(strings.TrimSuffix(out, "\n"), "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

func (e *pgEngine) export(ctx context.Context, name, path string) (int64, error) {
	args := []string{"--format=custom", "--no-password", "--file=" + path, "--dbname=" + e.conninfo(name)}
	code, err := e.d.Exec.RunSpec(ctx, e.toolSpec(e.dump, args, e.env()...))
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("pg_dump a échoué (code %d)", code)
	}
	size, err := checkHeader(path, "PGDMP")
	if err != nil {
		return 0, err
	}
	// Relecture complète : pg_restore lit chaque bloc de données (décompression comprise)
	// et écrit le script SQL dans le néant.
	code, err = e.d.Exec.RunSpec(ctx, e.toolSpec(e.restore, []string{"--file=" + os.DevNull, path}))
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("pg_restore ne relit pas l'export (code %d) : fichier incomplet ou altéré", code)
	}
	return size, nil
}

// after exporte les rôles et tablespaces (sans mots de passe) dans globals.sql : sans
// eux, recharger un export sur un serveur neuf échoue sur ses OWNER et GRANT.
func (e *pgEngine) after(ctx context.Context, dir string) string {
	const hint = "recréer les rôles à la main avant de recharger un export sur un autre serveur"
	if e.dumpall == "" {
		return "rôles non exportés : pg_dumpall absent du dossier de pg_dump ; " + hint
	}
	path := filepath.Join(dir, "globals.sql")
	args := []string{"--globals-only", "--no-role-passwords", "--no-password", "--file=" + path, "--dbname=" + e.conninfo("postgres")}
	code, err := e.d.Exec.RunSpec(ctx, e.toolSpec(e.dumpall, args, e.env()...))
	if err == nil && code != 0 {
		err = fmt.Errorf("code %d", code)
	}
	if err != nil {
		os.Remove(path)
		return fmt.Sprintf("rôles non exportés (pg_dumpall : %v ; voir le journal) ; %s", err, hint)
	}
	return ""
}

// checkHeader : le fichier existe, n'est pas vide et commence par magic ; renvoie sa taille.
func checkHeader(path, magic string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("export absent : %w", err)
	}
	defer f.Close()
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(f, head); err != nil || string(head) != magic {
		return 0, fmt.Errorf("export illisible : en-tête %s attendu", magic)
	}
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// firstLine : première ligne d'une sortie, pour un message.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
