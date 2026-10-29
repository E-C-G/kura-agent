package prepare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Export de bases (db_export) : l'agent lance l'outil d'export du moteur, une base après
// l'autre, et écrit un fichier par base dans le dossier de la préparation. L'export est
// cohérent sans arrêter la base : une seule transaction de lecture (PostgreSQL, MySQL),
// ou la sauvegarde native de SQL Server.
//
// Le mot de passe ne passe jamais en argument (visible dans la liste des processus) :
// variable d'environnement du seul processus lancé (PostgreSQL, SQL Server), ou fichier
// d'options de la zone de préparation, effacé après usage (MySQL).

// dbEngine : ce qui change d'un moteur à l'autre.
type dbEngine interface {
	// check vérifie les outils (droits, version) et renvoie la version lue.
	check(ctx context.Context) (string, error)
	// open prépare la connexion (fichier d'options…) ; close la défait.
	open() error
	close()
	// list renvoie les bases du serveur, hors bases système, dans l'ordre du serveur.
	list(ctx context.Context) ([]string, error)
	// ext : extension du fichier d'export (".dump"…).
	ext() string
	// export écrit la base name dans path et le contrôle ; renvoie sa taille.
	export(ctx context.Context, name, path string) (int64, error)
	// after : fin des exports réussis (PostgreSQL : rôles) ; renvoie un avertissement.
	after(ctx context.Context, dir string) string
}

// Remplaçables en test : contrôle des droits d'un outil (les outils de test ne sont pas
// à root), droit d'écriture du service SQL Server.
var (
	checkToolFile   = CheckTool
	grantSQLService = msGrant
)

// dbRun : une préparation db_export en cours.
type dbRun struct {
	p        config.Prepare
	d        Deps
	cfg      *config.Config
	password string
	log      *slog.Logger
	// seal, s'il est donné, chiffre chaque export au fil de l'eau (age) : rien n'est écrit
	// en clair, sauf la sauvegarde SQL Server, que le serveur écrit lui-même (chiffrée
	// puis effacée aussitôt).
	seal func(w io.Writer) (io.WriteCloser, error)
}

// sealExt : extension ajoutée aux exports chiffrés.
const sealExt = ".age"

// sealedFile crée path et renvoie l'écrivain qui chiffre vers lui ; Close ferme les deux.
func (r *dbRun) sealedFile(path string) (io.WriteCloser, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	w, err := r.seal(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &sealedWriter{WriteCloser: w, f: f}, nil
}

type sealedWriter struct {
	io.WriteCloser
	f *os.File
}

func (s *sealedWriter) Close() error { return errors.Join(s.WriteCloser.Close(), s.f.Close()) }

// sealPath chiffre le fichier clair vers path puis l'efface (réussi ou non).
func (r *dbRun) sealPath(ctx context.Context, clear, path string) (int64, error) {
	defer os.Remove(clear)
	in, err := os.Open(clear)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	w, err := r.sealedFile(path)
	if err != nil {
		return 0, err
	}
	_, err = copyCtx(ctx, w, in)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("chiffrement : %w", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// toolSpec : lancement d'un outil d'export : environnement réduit, dossier de travail
// dans la zone de préparation (aucune DLL d'un dossier d'utilisateur), sortie d'erreur
// au journal de la préparation.
func (r *dbRun) toolSpec(name string, args []string, extraEnv ...string) execx.Spec {
	return execx.Spec{Name: name, Args: args, Env: append(toolEnv(r.d.StagingDir), extraEnv...), Dir: r.d.StagingDir, Out: r.d.Out}
}

// capture lance un outil et renvoie sa sortie standard (sortie d'erreur au journal).
func (r *dbRun) capture(ctx context.Context, s execx.Spec) (string, int, error) {
	var out limitedBuffer
	s.Out, s.ErrOut = &out, r.d.Out
	code, err := r.d.Exec.RunSpec(ctx, s)
	if out.over {
		return "", code, fmt.Errorf("%s : sortie trop longue", filepath.Base(s.Name))
	}
	return out.String(), code, err
}

func newEngine(r *dbRun) (dbEngine, error) {
	switch r.p.Engine {
	case config.EnginePostgreSQL:
		return &pgEngine{dbRun: r, dump: r.cfg.Tools.PgDump}, nil
	case config.EngineMySQL:
		return &myEngine{dbRun: r, dump: r.cfg.Tools.MySQLDump}, nil
	case config.EngineSQLServer:
		return &msEngine{dbRun: r, sqlcmd: r.cfg.Tools.SQLCmd}, nil
	}
	return nil, fmt.Errorf("moteur %q inconnu", r.p.Engine)
}

// runDBExport fait une préparation db_export dans tmp.
func runDBExport(ctx context.Context, cfg *config.Config, p config.Prepare, d Deps, tmp string, ps *state.PrepareStatus, log *slog.Logger) error {
	return runDB(ctx, cfg, p, d, tmp, ps, log, nil)
}

// runDB fait l'export de p dans tmp ; seal, s'il est donné, chiffre chaque export au fil
// de l'eau (préparation age dont l'entrée est p).
func runDB(ctx context.Context, cfg *config.Config, p config.Prepare, d Deps, tmp string, ps *state.PrepareStatus, log *slog.Logger,
	seal func(io.Writer) (io.WriteCloser, error)) error {
	if d.LastSize > 0 {
		// Le dernier export réussi, plus 20 %, en plus de la réserve : sinon rien n'est lancé.
		need := MinFree + uint64(d.LastSize)/5*6
		if free, err := d.FreeSpace(d.StagingDir); err == nil && free < need {
			return &lowSpaceError{free: free, need: need, hint: fmt.Sprintf("dernier export %s, plus 20 %%, plus %s de réserve", humanSize(uint64(d.LastSize)), humanSize(MinFree))}
		}
	}
	r := &dbRun{p: p, d: d, cfg: cfg, log: log, seal: seal}
	if p.Password != "" {
		if d.Secret == nil {
			return errors.New("password : coffre des secrets indisponible")
		}
		v, err := d.Secret(p.Password)
		if err != nil {
			return fmt.Errorf("password (%s) : %w ; l'enregistrer avec « kura-agent credentials set %s »", p.Password,
				err, strings.TrimPrefix(p.Password, config.CredentialPrefix))
		}
		r.password = v
	}
	e, err := newEngine(r)
	if err != nil {
		return err
	}
	version, err := e.check(ctx)
	if err != nil {
		return err
	}
	log.Info("outil d'export", "moteur", p.Engine, "version", version)
	if err := e.open(); err != nil {
		return err
	}
	defer e.close()

	names := p.Databases
	if len(names) == 0 {
		all, err := e.list(ctx)
		if err != nil {
			return fmt.Errorf("liste des bases : %w", err)
		}
		if len(all) == 0 {
			return errors.New("aucune base à exporter sur le serveur (hors bases système) : vérifier les droits du rôle")
		}
		names = all
		log.Info("bases trouvées sur le serveur", "bases", names)
	}
	ext := e.ext()
	if seal != nil {
		ext += sealExt
	}
	files, err := exportFiles(names, ext)
	if err != nil {
		return err
	}
	var failed []string
	for i, name := range names {
		path := filepath.Join(tmp, files[i])
		fmt.Fprintf(d.Out, "== export de la base %q vers %s ==\n", name, files[i])
		n, err := e.export(ctx, name, path)
		if err != nil {
			os.Remove(path)
			if ctx.Err() != nil {
				return err
			}
			log.Error("export en échec", "base", name, "err", err)
			failed = append(failed, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		log.Info("base exportée", "base", name, "fichier", files[i], "octets", n)
		ps.Files++
		ps.Size += n
		ps.Databases = append(ps.Databases, name)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d base(s) sur %d en échec : %s ; voir le journal de la préparation", len(failed), len(names), sample(failed))
	}
	if warn := e.after(ctx, tmp); warn != "" {
		ps.Result, ps.Message = state.ResultWarning, warn
		log.Warn("préparation", "avertissement", warn)
	}
	return nil
}

// CheckDBTools vérifie les outils d'une préparation db_export (droits, version), comme
// avant chaque export ; renvoie la version lue.
func CheckDBTools(ctx context.Context, r Runner, cfg *config.Config, p config.Prepare) (string, error) {
	e, err := newEngine(&dbRun{p: p, d: Deps{Exec: r, StagingDir: os.TempDir(), Out: io.Discard}, cfg: cfg})
	if err != nil {
		return "", err
	}
	return e.check(ctx)
}

// CheckDBConnection se connecte au serveur avec les réglages de la préparation et renvoie
// les bases qu'elle exportera (validate, install). secret lit le mot de passe.
func CheckDBConnection(ctx context.Context, r Runner, cfg *config.Config, p config.Prepare, staging string, secret func(string) (string, error)) ([]string, error) {
	run := &dbRun{p: p, d: Deps{Exec: r, StagingDir: staging, Out: io.Discard}, cfg: cfg}
	if p.Password != "" {
		v, err := secret(p.Password)
		if err != nil {
			return nil, fmt.Errorf("password (%s) : %w", p.Password, err)
		}
		run.password = v
	}
	e, err := newEngine(run)
	if err != nil {
		return nil, err
	}
	if _, err := e.check(ctx); err != nil {
		return nil, err
	}
	if err := e.open(); err != nil {
		return nil, err
	}
	defer e.close()
	all, err := e.list(ctx)
	if err != nil {
		return nil, err
	}
	if len(p.Databases) == 0 {
		return all, nil
	}
	for _, want := range p.Databases {
		found := false
		for _, have := range all {
			found = found || have == want || p.Engine == config.EngineSQLServer && strings.EqualFold(have, want)
		}
		if !found {
			return nil, fmt.Errorf("base %q introuvable sur le serveur, ou invisible pour ce rôle (bases visibles : %s)", want, sample(all))
		}
	}
	return p.Databases, nil
}

// exportFiles : nom du fichier de chaque base. Les noms sont ceux de la base, sauf les
// caractères qu'un système de fichiers refuserait, codés %XX (UTF-8) : le nom reste lisible
// et se relit sans ambiguïté. Deux bases dont les fichiers ne diffèreraient que par la
// casse (Windows ne les distingue pas) sont refusées.
func exportFiles(names []string, ext string) ([]string, error) {
	files := make([]string, len(names))
	seen := map[string]string{}
	for i, n := range names {
		f := dbFileName(n) + ext
		key := strings.ToLower(f)
		if other, dup := seen[key]; dup {
			return nil, fmt.Errorf("les bases %q et %q donneraient le même fichier %s (la casse ne compte pas sous Windows) : les lister séparément dans deux préparations", other, n, f)
		}
		seen[key] = n
		files[i] = f
	}
	return files, nil
}

// dbFileName : nom de fichier tiré du nom de la base (voir exportFiles).
func dbFileName(name string) string {
	runes := []rune(name)
	var b strings.Builder
	for i, c := range runes {
		inner := i > 0 && i < len(runes)-1
		switch {
		case c != unicode.ReplacementChar && (unicode.IsLetter(c) || unicode.IsDigit(c)), c == '_', c == '-',
			(c == '.' || c == ' ') && inner:
			b.WriteRune(c)
		default:
			for _, by := range []byte(string(c)) {
				fmt.Fprintf(&b, "%%%02X", by)
			}
		}
	}
	s := b.String()
	if reservedFile(s) {
		// « con.dump » désigne un périphérique sous Windows : première lettre codée.
		s = fmt.Sprintf("%%%02X", s[0]) + s[1:]
	}
	return s
}

// reservedFile : nom de périphérique Windows (con, nul, com1…), quelle que soit la casse.
func reservedFile(s string) bool {
	l := strings.ToLower(s)
	switch l {
	case "con", "prn", "aux", "nul":
		return true
	}
	return len(l) == 4 && (strings.HasPrefix(l, "com") || strings.HasPrefix(l, "lpt")) && l[3] >= '0' && l[3] <= '9'
}

// sibling : outil du même dossier que tool, au premier nom présent de names (avec .exe
// sous Windows) ; "" si aucun.
func sibling(tool string, names ...string) string {
	dir := filepath.Dir(tool)
	for _, n := range names {
		if runtime.GOOS == "windows" {
			n += ".exe"
		}
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// toolVersion lance tool avec args et renvoie sa sortie (stdout et stderr mêlés).
func toolVersion(ctx context.Context, r Runner, tool string, args ...string) (string, error) {
	var out limitedBuffer
	_, err := r.RunSpec(ctx, execx.Spec{Name: tool, Args: args, Env: toolEnv(os.TempDir()), Dir: filepath.Dir(tool), Out: &out})
	if err != nil {
		return "", fmt.Errorf("lancement de %s : %w", tool, err)
	}
	return out.String(), nil
}

// versionAtLeast lit la version (majeure, mineure) trouvée par re dans out et la compare
// au minimum.
func versionAtLeast(out string, re *regexp.Regexp, major, minor int) (string, bool, bool) {
	m := re.FindStringSubmatch(out)
	if m == nil {
		return "", false, false
	}
	maj, _ := strconv.Atoi(m[1])
	mnr := 0
	if len(m) > 2 && m[2] != "" {
		mnr, _ = strconv.Atoi(m[2])
	}
	v := strconv.Itoa(maj) + "." + strconv.Itoa(mnr)
	return v, true, maj > major || maj == major && mnr >= minor
}

// limitedBuffer garde au plus 16 Mo de sortie (liste des bases, version).
type limitedBuffer struct {
	bytes.Buffer
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	const max = 16 << 20
	if b.Len()+len(p) > max {
		b.over = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// tailBuffer garde les derniers octets écrits (dernière ligne d'un export MySQL).
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// lastLine : dernière ligne non vide.
func (t *tailBuffer) lastLine() string {
	s := strings.TrimRight(string(t.buf), "\r\n \t")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}
