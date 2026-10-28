package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// gpgVersion : « gpg (GnuPG) 2.4.5 » ; 2.2 au moins.
var gpgVersion = regexp.MustCompile(`\(GnuPG[^)]*\)\s+(\d+)\.(\d+)`)

// gpgWarnBefore : une clé qui expire dans ce délai donne un avertissement au bilan.
const gpgWarnBefore = 30 * 24 * time.Hour

// CheckGPG vérifie tools.gpg (droits, GnuPG 2.2 ou plus) et renvoie sa version.
func CheckGPG(ctx context.Context, r Runner, path string) (string, error) {
	if err := checkToolFile(path); err != nil {
		return "", err
	}
	out, err := toolVersion(ctx, r, path, "--version")
	if err != nil {
		return "", err
	}
	v, found, ok := versionAtLeast(out, gpgVersion, 2, 2)
	switch {
	case !found:
		return "", fmt.Errorf("%s n'est pas gpg de GnuPG (version illisible : %q)", path, firstLine(out))
	case !ok:
		return v, fmt.Errorf("%s : GnuPG %s trop ancien (2.2 ou plus récent)", path, v)
	}
	return v, nil
}

// gpgRun : un trousseau propre à la préparation, dans la zone de préparation (réservée à
// l'agent), recréé à chaque exécution depuis les clés de la configuration : aucun
// trousseau d'un compte n'est lu ni modifié.
type gpgRun struct {
	gpg   string
	home  string
	d     Deps
	log   *slog.Logger
	now   time.Time
	fprs  []string        // empreintes déclarées
	enc   map[string]bool // identifiants longs des (sous-)clés de chiffrement utilisables
	owner map[string]string
	warn  []string
}

// args : options communes ; --no-autostart : ni gpg-agent ni keyboxd (aucune clé privée
// à servir), donc aucun démon qui survivrait à la préparation.
func (g *gpgRun) args(more ...string) []string {
	return append([]string{"--homedir", g.home, "--batch", "--no-tty", "--no-autostart", "--no-greeting"}, more...)
}

func (g *gpgRun) spec(args []string) execx.Spec {
	return execx.Spec{Name: g.gpg, Args: g.args(args...), Env: toolEnv(g.d.StagingDir), Dir: g.d.StagingDir, Out: g.d.Out}
}

// runGPG chiffre avec gpg pour les clés déclarées.
func runGPG(ctx context.Context, cfg *config.Config, p config.Prepare, d Deps, tmp string, ps *state.PrepareStatus, log *slog.Logger) error {
	version, err := CheckGPG(ctx, d.Exec, cfg.Tools.GPG)
	if err != nil {
		return fmt.Errorf("tools.gpg : %w", err)
	}
	log.Info("outil gpg", "chemin", cfg.Tools.GPG, "version", version)
	g := &gpgRun{gpg: cfg.Tools.GPG, home: filepath.Join(d.StagingDir, p.ID+".gnupg"), d: d, log: log, now: d.Now()}
	defer g.close()
	if err := g.keyring(ctx, cfg.RecipientsOf(p)); err != nil {
		return err
	}
	for _, fp := range g.fprs {
		ps.Recipients = append(ps.Recipients, fp)
	}
	if err := encryptAll(ctx, p, d, tmp, ps, g); err != nil {
		return err
	}
	if len(g.warn) > 0 {
		ps.Result, ps.Message = state.ResultWarning, strings.Join(g.warn, " ; ")
	}
	return nil
}

// keyring crée le trousseau, y importe chaque clé déclarée et la vérifie : empreinte,
// rien d'autre que les clés déclarées, ni révoquée ni expirée, une (sous-)clé de
// chiffrement utilisable.
func (g *gpgRun) keyring(ctx context.Context, recips []config.Recipient) error {
	if err := os.RemoveAll(g.home); err != nil {
		return err
	}
	if err := os.Mkdir(g.home, 0o700); err != nil {
		return fmt.Errorf("trousseau : %w", err)
	}
	// common.conf présent : GnuPG 2.4 n'en crée pas un qui lancerait keyboxd.
	if err := os.WriteFile(filepath.Join(g.home, "common.conf"), []byte("# kura-agent : trousseau sans démon\n"), 0o600); err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, r := range recips {
		fp := config.NormalizeFingerprint(r.Fingerprint)
		declared[fp] = true
		g.fprs = append(g.fprs, fp)
		s := g.spec([]string{"--import-options", "import-minimal", "--import"})
		s.Stdin = strings.NewReader(r.Key)
		if _, err := g.d.Exec.RunSpec(ctx, s); err != nil {
			return fmt.Errorf("import de la clé %s : %w", fp, err)
		}
	}
	var out limitedBuffer
	s := g.spec([]string{"--with-colons", "--fixed-list-mode", "--with-fingerprint", "--with-subkey-fingerprint", "--list-keys"})
	s.Out, s.ErrOut = &out, g.d.Out
	if _, err := g.d.Exec.RunSpec(ctx, s); err != nil {
		return err
	}
	keys := parseGPGColons(out.String())
	g.enc, g.owner = map[string]bool{}, map[string]string{}
	for _, k := range keys {
		if !declared[k.fpr] {
			return fmt.Errorf("la clé %s se trouve dans une clé fournie sans être déclarée : une clé par entrée de recipients", k.fpr)
		}
	}
	for _, fp := range g.fprs {
		k, ok := keys[fp]
		if !ok {
			return fmt.Errorf("clé %s : empreinte absente de la clé fournie (key), ou clé illisible ; voir le journal de la préparation", fp)
		}
		if err := g.usable(k); err != nil {
			return fmt.Errorf("clé %s : %w", fp, err)
		}
	}
	return nil
}

// gpgKey : une clé lue par --with-colons (clé principale ou sous-clé).
type gpgKey struct {
	fpr, keyid string
	validity   string // r : révoquée ; e : expirée
	expires    time.Time
	caps       string
	subs       []gpgKey
}

// parseGPGColons lit la sortie --with-colons --fixed-list-mode : clés principales par
// empreinte, avec leurs sous-clés.
func parseGPGColons(out string) map[string]gpgKey {
	keys := map[string]gpgKey{}
	var cur *gpgKey
	var sub *gpgKey
	flush := func() {
		if cur != nil && cur.fpr != "" {
			keys[cur.fpr] = *cur
		}
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), ":")
		if len(f) < 10 {
			continue
		}
		switch f[0] {
		case "pub":
			flush()
			cur, sub = &gpgKey{validity: f[1], keyid: f[4], expires: epoch(f[6]), caps: field(f, 11)}, nil
		case "sub":
			if cur != nil {
				cur.subs = append(cur.subs, gpgKey{validity: f[1], keyid: f[4], expires: epoch(f[6]), caps: field(f, 11)})
				sub = &cur.subs[len(cur.subs)-1]
			}
		case "fpr":
			switch {
			case sub != nil && sub.fpr == "":
				sub.fpr = strings.ToUpper(f[9])
			case cur != nil && cur.fpr == "":
				cur.fpr = strings.ToUpper(f[9])
			}
		}
	}
	flush()
	return keys
}

func field(f []string, i int) string {
	if i < len(f) {
		return f[i]
	}
	return ""
}

func epoch(s string) time.Time {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// usable vérifie la clé et note ses (sous-)clés de chiffrement utilisables.
func (g *gpgRun) usable(k gpgKey) error {
	switch {
	case k.validity == "r":
		return errors.New("révoquée")
	case k.validity == "e" || !k.expires.IsZero() && k.expires.Before(g.now):
		return fmt.Errorf("expirée le %s", k.expires.Format("02/01/2006"))
	}
	var last time.Time // dernière échéance parmi les clés de chiffrement utilisables
	consider := func(c gpgKey, owner string) {
		if !strings.Contains(c.caps, "e") || c.validity == "r" || c.validity == "e" ||
			!c.expires.IsZero() && c.expires.Before(g.now) {
			return
		}
		g.enc[strings.ToUpper(c.keyid)] = true
		g.owner[strings.ToUpper(c.keyid)] = owner
		if c.expires.IsZero() {
			last = time.Time{}.AddDate(9999, 0, 0)
		} else if c.expires.After(last) {
			last = c.expires
		}
	}
	// Capacités propres d'une clé : lettres minuscules (les majuscules résument la clé
	// entière, sous-clés comprises).
	consider(gpgKey{keyid: k.keyid, caps: k.caps, expires: k.expires}, k.fpr)
	for _, s := range k.subs {
		consider(s, k.fpr)
	}
	if last.IsZero() {
		return errors.New("aucune sous-clé de chiffrement valide (révoquées ou expirées)")
	}
	exp := last
	if !k.expires.IsZero() && k.expires.Before(exp) {
		exp = k.expires
	}
	if exp.Before(g.now.Add(gpgWarnBefore)) {
		g.warn = append(g.warn, fmt.Sprintf("clé gpg %s : expire le %s, à prolonger ou remplacer", k.fpr, exp.Format("02/01/2006")))
	}
	return nil
}

func (g *gpgRun) ext() string { return ".gpg" }

// file chiffre r dans dst, puis vérifie que dst porte un paquet de chiffrement par clé
// déclarée et rien d'autre. --trust-model always ne fait confiance qu'aux clés déclarées,
// seules présentes dans ce trousseau.
func (g *gpgRun) file(ctx context.Context, r io.Reader, dst, name string) error {
	args := []string{"--trust-model", "always"}
	if compressed(name) {
		args = append(args, "--compress-algo", "none")
	}
	for _, fp := range g.fprs {
		args = append(args, "--recipient", fp)
	}
	args = append(args, "--output", dst, "--encrypt")
	s := g.spec(args)
	s.Stdin = r
	code, err := g.d.Exec.RunSpec(ctx, s)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("gpg a échoué (code %d)", code)
	}
	return g.checkPackets(ctx, dst)
}

var pubkeyPacket = regexp.MustCompile(`:pubkey enc packet:.*keyid ([0-9A-Fa-f]{16})`)

// checkPackets : un paquet de chiffrement pour chaque clé déclarée, et seulement elles.
func (g *gpgRun) checkPackets(ctx context.Context, path string) error {
	var out limitedBuffer
	s := g.spec([]string{"--list-packets", path})
	s.Out, s.ErrOut = &out, g.d.Out
	if _, err := g.d.Exec.RunSpec(ctx, s); err != nil { // sans clé privée, gpg finit en erreur : normal
		return err
	}
	got := map[string]bool{}
	for _, m := range pubkeyPacket.FindAllStringSubmatch(out.String(), -1) {
		id := strings.ToUpper(m[1])
		owner, ok := g.owner[id]
		if !ok || !g.enc[id] {
			return fmt.Errorf("chiffré pour une clé non déclarée (%s)", id)
		}
		got[owner] = true
	}
	var missingFor []string
	for _, fp := range g.fprs {
		if !got[fp] {
			missingFor = append(missingFor, fp)
		}
	}
	if len(missingFor) > 0 {
		sort.Strings(missingFor)
		return fmt.Errorf("chiffré sans la clé %s", strings.Join(missingFor, ", "))
	}
	return nil
}

// close arrête les éventuels démons du trousseau (gpgconf du même dossier) puis l'efface.
func (g *gpgRun) close() {
	if conf := sibling(g.gpg, "gpgconf"); conf != "" && checkToolFile(conf) == nil {
		s := execx.Spec{Name: conf, Args: []string{"--homedir", g.home, "--kill", "all"}, Env: toolEnv(g.d.StagingDir),
			Dir: g.d.StagingDir, Out: g.d.Out}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		g.d.Exec.RunSpec(ctx, s)
		cancel()
	}
	if err := os.RemoveAll(g.home); err != nil {
		g.log.Warn("trousseau gpg non effacé", "dossier", g.home, "err", err)
	}
}

// CheckGPGKeys monte le trousseau de la préparation p dans dir (dossier temporaire) et
// vérifie ses clés comme avant chaque chiffrement (validate, install) ; renvoie les
// empreintes et les avertissements (expiration proche).
func CheckGPGKeys(ctx context.Context, r Runner, cfg *config.Config, p config.Prepare, dir string) ([]string, []string, error) {
	g := &gpgRun{gpg: cfg.Tools.GPG, home: filepath.Join(dir, p.ID+".gnupg"), now: time.Now(),
		d: Deps{Exec: r, StagingDir: dir, Out: io.Discard}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	defer g.close()
	if err := g.keyring(ctx, cfg.RecipientsOf(p)); err != nil {
		return nil, nil, err
	}
	return g.fprs, g.warn, nil
}
