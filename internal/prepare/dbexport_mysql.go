package prepare

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// myEngine : MySQL et MariaDB, par mysqldump ou mariadb-dump (une transaction de lecture
// par base), et mysql ou mariadb (liste des bases), pris dans le même dossier. L'export
// passe par l'agent, qui le compresse (gzip) et garde sa dernière ligne pour le contrôle.
type myEngine struct {
	*dbRun
	dump, client string
	mariadb      bool   // outil de MariaDB (sinon MySQL)
	major        int    // version majeure de l'outil MySQL
	options      string // fichier d'options de la connexion (mot de passe compris)
	nonTx        map[string]bool
}

var (
	// mysqldump  Ver 8.4.3 for Linux… ; mysqldump  Ver 10.19 Distrib 10.11.6-MariaDB… ;
	// mariadb-dump from 11.4.4-MariaDB, client 10.19…
	myDistrib = regexp.MustCompile(`Distrib\s+(\d+)\.(\d+)`)
	myFrom    = regexp.MustCompile(`from\s+(\d+)\.(\d+)`)
	myVer     = regexp.MustCompile(`Ver\s+(\d+)\.(\d+)`)
)

// Bases système, jamais exportées d'office.
var mySystemDBs = []string{"information_schema", "performance_schema", "sys", "mysql"}

func (e *myEngine) check(ctx context.Context) (string, error) {
	if err := checkToolFile(e.dump); err != nil {
		return "", fmt.Errorf("tools.mysqldump : %w", err)
	}
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(e.dump)), ".exe")
	clients := []string{"mysql", "mariadb"}
	if strings.HasPrefix(base, "mariadb") {
		clients = []string{"mariadb", "mysql"}
	}
	if e.client = sibling(e.dump, clients...); e.client == "" {
		return "", fmt.Errorf("tools.mysqldump : client mysql ou mariadb absent du dossier de l'outil (%s)", filepath.Dir(e.dump))
	}
	if err := checkToolFile(e.client); err != nil {
		return "", fmt.Errorf("tools.mysqldump : %w", err)
	}
	out, err := toolVersion(ctx, e.d.Exec, e.dump, "--version")
	if err != nil {
		return "", fmt.Errorf("tools.mysqldump : %w", err)
	}
	e.mariadb = strings.Contains(strings.ToLower(out), "mariadb")
	// « Distrib » donne la version du serveur dont vient l'outil (MySQL 5.7, MariaDB 10) ;
	// sinon « from » (MariaDB 11) ou « Ver » (MySQL 8).
	re, min := myVer, 5
	switch {
	case myDistrib.MatchString(out):
		re = myDistrib
	case e.mariadb:
		re = myFrom
	}
	if e.mariadb {
		min = 10
	}
	v, found, ok := versionAtLeast(out, re, min, 0)
	switch {
	case !found:
		return "", fmt.Errorf("tools.mysqldump : %s n'est ni mysqldump ni mariadb-dump (version illisible : %q)", e.dump, firstLine(out))
	case !ok:
		return v, fmt.Errorf("tools.mysqldump : version %s trop ancienne", v)
	}
	e.major, _ = strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if e.mariadb {
		return "MariaDB " + v, nil
	}
	return "MySQL " + v, nil
}

func (e *myEngine) ext() string { return ".sql.gz" }

// open écrit le fichier d'options de la connexion dans la zone de préparation (réservée
// à l'agent), hors du dossier envoyé au bucket : le mot de passe n'apparaît ni dans la
// liste des processus ni dans l'environnement.
func (e *myEngine) open() error {
	e.options = filepath.Join(e.d.StagingDir, e.p.ID+".cnf")
	os.Remove(e.options)
	var b strings.Builder
	b.WriteString("[client]\n")
	opt := func(k, v string) { fmt.Fprintf(&b, "%s=\"%s\"\n", k, myEscape(v)) }
	if e.p.Socket != "" {
		opt("socket", e.p.Socket)
	} else {
		opt("host", e.p.Host)
		fmt.Fprintf(&b, "port=%d\n", e.p.Port)
	}
	if e.p.User != "" {
		opt("user", e.p.User)
	}
	if e.password != "" {
		opt("password", e.password)
	}
	f, err := os.OpenFile(e.options, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("fichier d'options : %w", err)
	}
	_, err = f.WriteString(b.String())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(e.options)
		return fmt.Errorf("fichier d'options : %w", err)
	}
	return nil
}

func (e *myEngine) close() {
	if e.options != "" {
		os.Remove(e.options)
	}
}

// myEscape : valeur entre guillemets d'un fichier d'options MySQL. Les outils retirent
// les guillemets du bord et lisent les séquences \\, \n, \r, \t, \b ; un guillemet
// intérieur reste tel quel (« \" » serait lu comme deux caractères).
func myEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\b", `\b`).Replace(v)
}

// args : le fichier d'options vient toujours en premier (exigé par les outils).
func (e *myEngine) args(more ...string) []string {
	return append([]string{"--defaults-extra-file=" + e.options}, more...)
}

func (e *myEngine) list(ctx context.Context) ([]string, error) {
	// Bases visibles, et celles qui ont des tables non transactionnelles (MyISAM, Aria…),
	// que --single-transaction ne fige pas : signalées au journal.
	q := "SELECT CONCAT('D:', SCHEMA_NAME) FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME; " +
		"SELECT DISTINCT CONCAT('M:', TABLE_SCHEMA) FROM information_schema.TABLES " +
		"WHERE TABLE_TYPE = 'BASE TABLE' AND ENGINE IS NOT NULL AND ENGINE <> 'InnoDB'"
	out, code, err := e.capture(ctx, e.toolSpec(e.client, e.args("--batch", "--skip-column-names", "-e", q)))
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%s a échoué (code %d) : connexion ou droits ; voir le journal de la préparation", filepath.Base(e.client), code)
	}
	var names []string
	e.nonTx = map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(out, "\r\n"), "\n") {
		v := myUnescape(strings.TrimRight(line, "\r"))
		switch {
		case strings.HasPrefix(v, "D:") && !slices.Contains(mySystemDBs, strings.ToLower(v[2:])):
			names = append(names, v[2:])
		case strings.HasPrefix(v, "M:"):
			e.nonTx[v[2:]] = true
		}
	}
	return names, nil
}

// myUnescape défait l'échappement du mode --batch (\\, \n, \t, \0).
func myUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i == len(s)-1 {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func (e *myEngine) export(ctx context.Context, name, path string) (int64, error) {
	if e.nonTx == nil {
		// Bases listées dans la configuration : la liste n'a pas encore été lue.
		if _, err := e.list(ctx); err != nil {
			return 0, err
		}
	}
	if e.nonTx[name] {
		e.log.Warn("tables non transactionnelles (MyISAM, Aria…) : leur export n'est pas figé si elles changent pendant l'export", "base", name)
	}
	args := []string{"--single-transaction", "--routines", "--triggers", "--events", "--hex-blob",
		"--no-tablespaces", "--default-character-set=utf8mb4"}
	if !e.mariadb && e.major >= 8 {
		// Sans effet sur les données (histogrammes) ; évite l'échec de mysqldump 8 face à
		// un serveur qui n'a pas COLUMN_STATISTICS (MySQL 5.7).
		args = append(args, "--column-statistics=0")
	}
	if !e.mariadb {
		// Ni SET @@GLOBAL.GTID_PURGED dans l'export (il empêcherait de le recharger sur un
		// autre serveur), ni droit RELOAD exigé quand le serveur a gtid_mode=ON.
		args = append(args, "--set-gtid-purged=OFF")
	}
	args = append(args, "--databases", name)

	// Fichier clair, ou chiffré au fil de l'eau (age) : compressé avant d'être chiffré.
	var f io.WriteCloser
	var err error
	if e.seal != nil {
		f, err = e.sealedFile(path)
	} else {
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if err != nil {
		return 0, err
	}
	gz := gzip.NewWriter(f)
	tail := &tailBuffer{max: 512}
	s := e.toolSpec(e.dump, e.args(args...))
	s.Out, s.ErrOut = io.MultiWriter(gz, tail), e.d.Out
	code, runErr := e.d.Exec.RunSpec(ctx, s)
	err = errors.Join(runErr, gz.Close(), f.Close())
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("%s a échoué (code %d)", filepath.Base(e.dump), code)
	}
	if last := tail.lastLine(); !strings.HasPrefix(last, "-- Dump completed") {
		return 0, fmt.Errorf("export incomplet : dernière ligne %q au lieu de « -- Dump completed »", firstLine(last))
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (e *myEngine) after(context.Context, string) string { return "" }
