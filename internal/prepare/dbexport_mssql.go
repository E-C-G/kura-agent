package prepare

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf16"
)

// msEngine : SQL Server (Windows), par sqlcmd : le serveur écrit lui-même la sauvegarde
// (BACKUP DATABASE … WITH COPY_ONLY, CHECKSUM) dans le dossier de la préparation, où son
// compte de service reçoit le droit d'écrire le temps de l'export (msGrant).
type msEngine struct {
	*dbRun
	sqlcmd   string
	compress bool              // l'édition sait compresser une sauvegarde (ni Express, ni Web)
	states   map[string]string // base (en minuscules) → état (ONLINE, OFFLINE…)
	names    map[string]string // base (en minuscules) → nom exact
	order    []string          // bases, dans l'ordre du serveur
	revoke   func() error      // rend la zone de préparation à l'agent seul
}

var (
	// sqlcmd ODBC : « Version 15.0.4298.1 NT » ; go-sqlcmd : « sqlcmd: v1.8.2 ».
	msVersionODBC = regexp.MustCompile(`(?i)version\s+v?(\d+)\.(\d+)`)
	msVersionGo   = regexp.MustCompile(`(?i)sqlcmd:?\s+v?(\d+)\.(\d+)`)
)

func (e *msEngine) check(ctx context.Context) (string, error) {
	if err := checkToolFile(e.sqlcmd); err != nil {
		return "", fmt.Errorf("tools.sqlcmd : %w", err)
	}
	for _, args := range [][]string{{"--version"}, {"-?"}} {
		out, err := toolVersion(ctx, e.d.Exec, e.sqlcmd, args...)
		if err != nil {
			return "", fmt.Errorf("tools.sqlcmd : %w", err)
		}
		for _, re := range []*regexp.Regexp{msVersionGo, msVersionODBC} {
			if v, found, _ := versionAtLeast(out, re, 0, 0); found {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("tools.sqlcmd : %s n'est pas sqlcmd (version illisible)", e.sqlcmd)
}

func (e *msEngine) ext() string { return ".bak" }

// conn : arguments de connexion. -b : arrêt et code de sortie au premier échec ;
// -x : aucune substitution de variables $(…) (un nom de base ne peut rien injecter) ;
// -C : certificat du serveur local accepté (instance locale seulement) ; -h -1 : sans
// en-têtes. Avec un utilisateur, le mot de passe passe par SQLCMDPASSWORD ; sans,
// authentification Windows du compte de l'agent (SYSTEM).
func (e *msEngine) conn(query string) []string {
	args := []string{"-S", e.p.Instance, "-b", "-x", "-C", "-l", "30", "-h", "-1"}
	if e.p.User != "" {
		args = append(args, "-U", e.p.User)
	} else {
		args = append(args, "-E")
	}
	return append(args, "-Q", query)
}

func (e *msEngine) env() []string {
	if e.password == "" {
		return nil
	}
	return []string{"SQLCMDPASSWORD=" + e.password}
}

// open : rien à préparer ; le droit d'écriture du service est donné au premier export.
func (e *msEngine) open() error { return nil }

func (e *msEngine) close() {
	if e.revoke != nil {
		if err := e.revoke(); err != nil {
			e.log.Error("droits de la zone de préparation non rétablis après SQL Server", "err", err)
		}
		e.revoke = nil
	}
}

// msInfoQuery : édition, puis chaque base utilisateur (nom en UTF-16 codé en hexadécimal :
// aucun caractère du nom ne peut gêner la lecture) et son état.
const msInfoQuery = "SET NOCOUNT ON; " +
	"SELECT 'E:' + CONVERT(varchar(128), SERVERPROPERTY('Edition')); " +
	"SELECT 'D:' + CONVERT(varchar(600), CONVERT(varbinary(300), name), 2) + ',' + state_desc " +
	"FROM sys.databases WHERE database_id > 4 ORDER BY name;"

func (e *msEngine) list(ctx context.Context) ([]string, error) {
	if err := e.info(ctx); err != nil {
		return nil, err
	}
	return append([]string(nil), e.order...), nil
}

func (e *msEngine) info(ctx context.Context) error {
	if e.states != nil {
		return nil
	}
	out, code, err := e.capture(ctx, e.toolSpec(e.sqlcmd, e.conn(msInfoQuery), e.env()...))
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("sqlcmd a échoué (code %d) : connexion ou droits ; voir le journal de la préparation", code)
	}
	e.states, e.names, e.order = map[string]string{}, map[string]string{}, nil
	edition := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "E:"):
			edition = line[2:]
		case strings.HasPrefix(line, "D:"):
			hexName, st, ok := strings.Cut(line[2:], ",")
			name, err := utf16Hex(hexName)
			if !ok || err != nil {
				return fmt.Errorf("liste des bases illisible (%q)", firstLine(line))
			}
			e.states[strings.ToLower(name)], e.names[strings.ToLower(name)] = st, name
			e.order = append(e.order, name)
		}
	}
	if edition == "" {
		return fmt.Errorf("édition de SQL Server illisible dans la réponse de sqlcmd (%q)", firstLine(out))
	}
	ed := strings.ToLower(edition)
	e.compress = !strings.Contains(ed, "express") && !strings.Contains(ed, "web")
	e.log.Info("SQL Server", "instance", e.p.Instance, "édition", edition, "compression", e.compress)
	return nil
}

// utf16Hex : nom NVARCHAR lu en hexadécimal (UTF-16LE).
func utf16Hex(s string) (string, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b)%2 != 0 {
		return "", fmt.Errorf("hexadécimal invalide")
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u)), nil
}

// msServiceName : service Windows d'une instance (localhost → MSSQLSERVER,
// localhost\SQLEXPRESS → MSSQL$SQLEXPRESS).
func msServiceName(instance string) string {
	s, _, _ := strings.Cut(instance, ",")
	if _, name, ok := strings.Cut(s, `\`); ok && !strings.EqualFold(name, "MSSQLSERVER") {
		return "MSSQL$" + strings.ToUpper(name)
	}
	return "MSSQLSERVER"
}

// msIdent : identifiant entre crochets, « ] » doublé.
func msIdent(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }

// msString : chaîne Unicode entre apostrophes, « ' » doublée.
func msString(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (e *msEngine) export(ctx context.Context, name, path string) (int64, error) {
	if e.seal != nil {
		// Le serveur écrit la sauvegarde en clair ; elle est chiffrée puis effacée aussitôt.
		clear := strings.TrimSuffix(path, sealExt)
		if _, err := e.backup(ctx, name, clear); err != nil {
			os.Remove(clear)
			return 0, err
		}
		return e.sealPath(ctx, clear, path)
	}
	return e.backup(ctx, name, path)
}

// backup : sauvegarde de la base name dans path, par le serveur, puis contrôlée.
func (e *msEngine) backup(ctx context.Context, name, path string) (int64, error) {
	if err := e.info(ctx); err != nil {
		return 0, err
	}
	st, ok := e.states[strings.ToLower(name)]
	switch {
	case !ok:
		return 0, fmt.Errorf("base introuvable sur l'instance %s, ou invisible pour ce compte", e.p.Instance)
	case st != "ONLINE":
		return 0, fmt.Errorf("base dans l'état %s, pas ONLINE : non sauvegardée", st)
	}
	if e.revoke == nil {
		revoke, err := grantSQLService(e.p.Instance, e.dir(path))
		if err != nil {
			return 0, fmt.Errorf("droit d'écriture du service SQL Server : %w", err)
		}
		e.revoke = revoke
	}
	with := "COPY_ONLY, CHECKSUM, INIT"
	if e.compress {
		with += ", COMPRESSION"
	}
	q := fmt.Sprintf("BACKUP DATABASE %s TO DISK = %s WITH %s;", msIdent(e.names[strings.ToLower(name)]), msString(path), with)
	code, err := e.d.Exec.RunSpec(ctx, e.toolSpec(e.sqlcmd, e.conn(q), e.env()...))
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("BACKUP DATABASE a échoué (code %d) ; voir le journal de la préparation", code)
	}
	if e.p.Verify {
		q := fmt.Sprintf("RESTORE VERIFYONLY FROM DISK = %s WITH CHECKSUM;", msString(path))
		code, err := e.d.Exec.RunSpec(ctx, e.toolSpec(e.sqlcmd, e.conn(q), e.env()...))
		if err != nil {
			return 0, err
		}
		if code != 0 {
			return 0, fmt.Errorf("RESTORE VERIFYONLY a échoué (code %d) : sauvegarde illisible", code)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("sauvegarde absente : %w", err)
	}
	if fi.Size() == 0 {
		return 0, fmt.Errorf("sauvegarde vide")
	}
	return fi.Size(), nil
}

func (e *msEngine) dir(path string) string { return path[:strings.LastIndexAny(path, `/\`)] }

func (e *msEngine) after(context.Context, string) string {
	// Droits rendus avant que le dossier ne devienne le résultat (renommé) : le compte du
	// service ne garde rien sur les sauvegardes.
	e.close()
	return ""
}
