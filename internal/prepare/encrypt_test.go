package prepare

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/E-C-G/kura-agent/internal/execx"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

func newAgeIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func decryptAge(t *testing.T, path string, id age.Identity) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := age.Decrypt(f, id)
	if err != nil {
		t.Fatalf("%s : %v", path, err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// listTree : fichiers sous dir, chemins relatifs en /.
func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(out)
	return out
}

func encFixture(t *testing.T, typ string, recips []config.Recipient) (*config.Config, string, string) {
	t.Helper()
	dir := t.TempDir()
	staging, src := filepath.Join(dir, "staging"), filepath.Join(dir, "Contrats")
	os.MkdirAll(staging, 0o700)
	for name, data := range map[string]string{"a.txt": "alpha", "sous/b.pdf": "bravo", "c.zip": "PK"} {
		p := filepath.Join(src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(data), 0o644)
	}
	cfg := &config.Config{Tools: config.Tools{GPG: filepath.Join(dir, "bin", "gpg")},
		Prepare: []config.Prepare{{ID: "contrats", Type: typ, Source: src, Recipients: recips}}}
	cfg.ApplyDefaults()
	return cfg, staging, src
}

func TestAgeSource(t *testing.T) {
	id := newAgeIdentity(t)
	cfg, staging, src := encFixture(t, config.PrepareAge, []config.Recipient{{Age: id.Recipient().String()}})
	outside := filepath.Join(filepath.Dir(src), "dehors")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)
	if runtime.GOOS != "windows" {
		os.Symlink(outside, filepath.Join(src, "lien"))
	}
	agent := filepath.Join(src, "Kura")
	os.MkdirAll(agent, 0o700)
	os.WriteFile(filepath.Join(agent, "coffre.secret"), []byte("k"), 0o600)

	ps := Run(context.Background(), cfg, cfg.Prepare[0], Deps{Exec: &fakeDB{}, StagingDir: staging, AgentDirs: []string{agent}})
	if ps.Result != state.ResultOK || ps.Files != 3 || len(ps.Recipients) != 1 {
		t.Fatalf("%+v", ps)
	}
	out := filepath.Join(staging, "contrats")
	if got := listTree(t, out); !slices.Equal(got, []string{"a.txt.age", "c.zip.age", "sous/b.pdf.age"}) {
		t.Fatalf("fichiers : %v", got)
	}
	if got := decryptAge(t, filepath.Join(out, "sous", "b.pdf.age"), id); got != "bravo" {
		t.Fatalf("déchiffré : %q", got)
	}
}

func TestAgeSingleFileAndRefusals(t *testing.T) {
	id := newAgeIdentity(t)
	cfg, staging, src := encFixture(t, config.PrepareAge, []config.Recipient{{Age: id.Recipient().String()}})
	cfg.Prepare[0].Source = filepath.Join(src, "a.txt")
	ps := Run(context.Background(), cfg, cfg.Prepare[0], Deps{Exec: &fakeDB{}, StagingDir: staging})
	if ps.Result != state.ResultOK || ps.Files != 1 {
		t.Fatalf("%+v", ps)
	}
	if got := decryptAge(t, filepath.Join(staging, "contrats", "a.txt.age"), id); got != "alpha" {
		t.Fatalf("%q", got)
	}
	// Source dans un dossier de l'agent : refusée.
	cfg.Prepare[0].Source = src
	ps = Run(context.Background(), cfg, cfg.Prepare[0], Deps{Exec: &fakeDB{}, StagingDir: staging, AgentDirs: []string{filepath.Dir(src)}})
	if ps.Result != state.ResultError || !strings.Contains(ps.Message, "dossier de l'agent") {
		t.Fatalf("%+v", ps)
	}
	// Identité privée dans la configuration : rien n'est chiffré.
	cfg.Prepare[0].Recipients = []config.Recipient{{Age: id.String()}}
	ps = Run(context.Background(), cfg, cfg.Prepare[0], Deps{Exec: &fakeDB{}, StagingDir: staging})
	if ps.Result != state.ResultError || strings.Contains(ps.Message, id.String()) {
		t.Fatalf("%+v", ps)
	}
}

// L'entrée (résultat d'une préparation) est chiffrée puis effacée.
func TestAgeInputDeletesClear(t *testing.T) {
	id := newAgeIdentity(t)
	cfg, staging, _ := encFixture(t, config.PrepareAge, []config.Recipient{{Age: id.Recipient().String()}})
	cfg.Prepare = []config.Prepare{{ID: "courrier", Type: config.PrepareZip}, {ID: "courrier-chiffre", Type: config.PrepareAge,
		Input: "courrier", Recipients: cfg.Prepare[0].Recipients}}
	clear := filepath.Join(staging, "courrier")
	os.MkdirAll(clear, 0o700)
	os.WriteFile(filepath.Join(clear, "courrier.zip"), []byte("PK archive"), 0o600)
	ps := Run(context.Background(), cfg, cfg.Prepare[1], Deps{Exec: &fakeDB{}, StagingDir: staging})
	if ps.Result != state.ResultOK || ps.Files != 1 {
		t.Fatalf("%+v", ps)
	}
	if _, err := os.Stat(clear); !os.IsNotExist(err) {
		t.Fatal("clair resté dans la zone")
	}
	if got := decryptAge(t, filepath.Join(staging, "courrier-chiffre", "courrier.zip.age"), id); got != "PK archive" {
		t.Fatalf("%q", got)
	}
}

// Un export chiffré par age passe directement dans le chiffrement : pg_dump n'écrit aucun
// fichier, seul le .age existe.
func TestAgeStreamsDBExport(t *testing.T) {
	id := newAgeIdentity(t)
	f := newDBFixture(t, config.EnginePostgreSQL, "pg_dump", "psql", "pg_restore", "pg_dumpall")
	f.cfg.Prepare = append(f.cfg.Prepare, config.Prepare{ID: "bases-chiffre", Type: config.PrepareAge, Input: "bases",
		Recipients: []config.Recipient{{Age: id.Recipient().String()}}})
	f.cfg.ApplyDefaults()
	db := &fakeDB{version: "pg_dump (PostgreSQL) 17.2", databases: []string{"compta"}}
	ps := Run(context.Background(), f.cfg, f.cfg.Prepare[1], Deps{Exec: db, StagingDir: f.staging,
		Secret: func(string) (string, error) { return "x", nil }})
	if ps.Result != state.ResultOK || ps.Files != 1 || !slices.Equal(ps.Databases, []string{"compta"}) {
		t.Fatalf("%+v", ps)
	}
	out := filepath.Join(f.staging, "bases-chiffre")
	if got := listTree(t, out); !slices.Equal(got, []string{"compta.dump.age", "globals.sql.age"}) {
		t.Fatalf("fichiers : %v", got)
	}
	if got := decryptAge(t, filepath.Join(out, "compta.dump.age"), id); !strings.HasPrefix(got, "PGDMP") {
		t.Fatalf("export déchiffré : %q", got)
	}
	for _, s := range db.calls("pg_dump") {
		if argValue(s.Args, "--file=") != "" {
			t.Fatalf("pg_dump écrit un fichier en clair : %v", s.Args)
		}
	}
	if len(db.calls("pg_restore")) != 0 {
		t.Error("relecture par pg_restore sans clair")
	}
}

// fakeGPG joue gpg : import, liste des clés (--with-colons), chiffrement (préfixe GPG:),
// liste des paquets.
type fakeGPG struct {
	mu      sync.Mutex
	colons  string   // sortie de --list-keys
	packets []string // identifiants longs annoncés par --list-packets ; nil : ceux de encIDs
	encIDs  []string
	specs   []execx.Spec
}

func (f *fakeGPG) RunSpec(ctx context.Context, s execx.Spec) (int, error) {
	f.mu.Lock()
	f.specs = append(f.specs, s)
	f.mu.Unlock()
	switch {
	case slices.Contains(s.Args, "--version"):
		io.WriteString(s.Out, "gpg (GnuPG) 2.4.4\nlibgcrypt 1.10.3\n")
	case slices.Contains(s.Args, "--import"):
		io.Copy(io.Discard, s.Stdin)
	case slices.Contains(s.Args, "--list-keys"):
		io.WriteString(s.Out, f.colons)
	case slices.Contains(s.Args, "--encrypt"):
		data, _ := io.ReadAll(s.Stdin)
		return 0, os.WriteFile(argValue2(s.Args, "--output"), append([]byte("GPG:"), data...), 0o600)
	case slices.Contains(s.Args, "--list-packets"):
		ids := f.packets
		if ids == nil {
			ids = f.encIDs
		}
		for _, id := range ids {
			fmt.Fprintf(s.Out, "# off=0 ctb=84 tag=1 hlen=2 plen=94\n:pubkey enc packet: version 3, algo 18, keyid %s\n", id)
		}
		return 2, nil // sans clé privée
	}
	return 0, nil
}

func argValue2(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

const (
	fprA = "0123456789ABCDEF0123456789ABCDEF01234567"
	subA = "1111222233334444"
	fprB = "89ABCDEF0123456789ABCDEF0123456789ABCDEF"
	subB = "5555666677778888"
)

func colonKey(fpr, sub string, expires time.Time, subValidity string) string {
	exp := ""
	if !expires.IsZero() {
		exp = fmt.Sprint(expires.Unix())
	}
	return fmt.Sprintf("pub:-:255:22:%s:1700000000:%s::-:::scESC:::::ed25519:::0:\nfpr:::::::::%s:\n"+
		"sub:%s:255:18:%s:1700000000:%s:::::e:::::cv25519::\nfpr:::::::::AAAA%s:\n",
		fpr[24:], exp, fpr, subValidity, sub, exp, sub)
}

func gpgFixture(t *testing.T) (*config.Config, string) {
	t.Helper()
	old := checkToolFile
	checkToolFile = func(string) error { return nil }
	t.Cleanup(func() { checkToolFile = old })
	cfg, staging, _ := encFixture(t, config.PrepareGPG, []config.Recipient{
		{Fingerprint: "0123 4567 89ab cdef 0123 4567 89AB CDEF 0123 4567", Key: "-----BEGIN PGP PUBLIC KEY BLOCK-----\nA\n-----END PGP PUBLIC KEY BLOCK-----"},
		{Fingerprint: fprB, Key: "-----BEGIN PGP PUBLIC KEY BLOCK-----\nB\n-----END PGP PUBLIC KEY BLOCK-----"},
	})
	return cfg, staging
}

func TestGPGEncrypt(t *testing.T) {
	cfg, staging := gpgFixture(t)
	g := &fakeGPG{colons: colonKey(fprA, subA, time.Time{}, "-") + colonKey(fprB, subB, time.Time{}, "-"), encIDs: []string{subA, subB}}
	ps := Run(context.Background(), cfg, cfg.Prepare[0], Deps{Exec: g, StagingDir: staging})
	if ps.Result != state.ResultOK || ps.Files != 3 || !slices.Equal(ps.Recipients, []string{fprA, fprB}) {
		t.Fatalf("%+v", ps)
	}
	out := filepath.Join(staging, "contrats")
	if got := listTree(t, out); !slices.Equal(got, []string{"a.txt.gpg", "c.zip.gpg", "sous/b.pdf.gpg"}) {
		t.Fatalf("fichiers : %v", got)
	}
	data, _ := os.ReadFile(filepath.Join(out, "a.txt.gpg"))
	if string(data) != "GPG:alpha" {
		t.Fatalf("%q", data)
	}
	var encrypts [][]string
	for _, s := range g.specs {
		if !slices.Contains(s.Args, "--homedir") || !slices.Contains(s.Args, "--no-autostart") {
			if !slices.Contains(s.Args, "--version") {
				t.Errorf("trousseau propre absent : %v", s.Args)
			}
		}
		if slices.Contains(s.Args, "--encrypt") {
			encrypts = append(encrypts, s.Args)
		}
	}
	for _, a := range encrypts {
		if !slices.Contains(a, fprA) || !slices.Contains(a, fprB) || !slices.Contains(a, "always") {
			t.Errorf("destinataires : %v", a)
		}
		zip := strings.HasSuffix(argValue2(a, "--output"), "c.zip.gpg")
		if slices.Contains(a, "--compress-algo") != zip {
			t.Errorf("compression : %v", a)
		}
	}
	if left, _ := os.ReadDir(staging); len(left) != 1 {
		t.Errorf("trousseau resté : %v", left)
	}
}

func TestGPGKeyProblems(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name    string
		colons  string
		packets []string
		result  string
		msg     string
	}{
		{"expire bientôt", colonKey(fprA, subA, now.Add(10*24*time.Hour), "-") + colonKey(fprB, subB, time.Time{}, "-"), nil,
			state.ResultWarning, "expire le"},
		{"expirée", colonKey(fprA, subA, now.Add(-time.Hour), "e") + colonKey(fprB, subB, time.Time{}, "-"), nil,
			state.ResultError, "expirée"},
		{"sous-clé révoquée", colonKey(fprA, subA, time.Time{}, "r") + colonKey(fprB, subB, time.Time{}, "-"), nil,
			state.ResultError, "aucune sous-clé de chiffrement"},
		{"empreinte absente", colonKey(fprB, subB, time.Time{}, "-"), nil, state.ResultError, "empreinte absente"},
		{"clé en trop", colonKey(fprA, subA, time.Time{}, "-") + colonKey(fprB, subB, time.Time{}, "-") +
			colonKey("FFFFEEEEDDDDCCCCBBBBAAAA9999888877776666", "9999999999999999", time.Time{}, "-"), nil,
			state.ResultError, "sans être déclarée"},
		{"paquet pour une autre clé", colonKey(fprA, subA, time.Time{}, "-") + colonKey(fprB, subB, time.Time{}, "-"),
			[]string{subA, subB, "DEADBEEFDEADBEEF"}, state.ResultError, "non déclarée"},
		{"paquet manquant", colonKey(fprA, subA, time.Time{}, "-") + colonKey(fprB, subB, time.Time{}, "-"),
			[]string{subA}, state.ResultError, "sans la clé " + fprB},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, staging := gpgFixture(t)
			g := &fakeGPG{colons: c.colons, packets: c.packets, encIDs: []string{subA, subB}}
			ps := Run(context.Background(), cfg, cfg.Prepare[0], Deps{Exec: g, StagingDir: staging})
			if ps.Result != c.result || !strings.Contains(ps.Message, c.msg) {
				t.Fatalf("%+v", ps)
			}
		})
	}
}

func TestParseGPGColons(t *testing.T) {
	keys := parseGPGColons(colonKey(fprA, subA, time.Unix(1900000000, 0), "-"))
	k, ok := keys[fprA]
	if !ok || len(k.subs) != 1 || k.subs[0].keyid != subA || k.subs[0].caps != "e" || k.expires.Unix() != 1900000000 {
		t.Fatalf("%+v", keys)
	}
	if !bytes.Contains([]byte(k.caps), []byte("E")) {
		t.Fatalf("capacités : %q", k.caps)
	}
}
