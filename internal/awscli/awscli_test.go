package awscli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/pkg/config"
)

var profile = config.Profile{
	Endpoint:          "https://s3.eu-central-1.wasabisys.com",
	Bucket:            "acme-srv01",
	ChecksumAlgorithm: "CRC32",
}

// La commande générée doit reproduire le script planifié actuel.
func TestSyncArgsMatchCurrentScript(t *testing.T) {
	data := config.Job{
		ID: "data", Source: "E:/", Destination: "DATA", Delete: true,
		Exclude: []string{"*$RECYCLE.BIN*", "*System Volume Information*"},
	}
	got := strings.Join(SyncArgs(profile, data, false), " ")
	want := "s3 sync E:/ s3://acme-srv01/DATA --endpoint-url https://s3.eu-central-1.wasabisys.com --no-progress --no-follow-symlinks " +
		"--exclude *$RECYCLE.BIN* --exclude *System Volume Information* --delete"
	if got != want {
		t.Errorf("commande :\n reçu   %s\n attendu %s", got, want)
	}

	outlook := config.Job{ID: "outlook", Source: "C:/Users/alice/AppData/Local/Microsoft/Outlook", Destination: "C/alice/Outlook", Delete: true}
	got = strings.Join(SyncArgs(profile, outlook, true), " ")
	if !strings.Contains(got, "s3://acme-srv01/C/alice/Outlook") || !strings.HasSuffix(got, "--delete --dryrun") {
		t.Errorf("commande outlook en simulation inattendue : %s", got)
	}
}

func TestEnvNeverPagesAndSetsProfile(t *testing.T) {
	env := Env(config.Profile{AWSProfile: "wasabi"})
	if strings.Join(env, " ") != "AWS_PAGER= PYTHONUTF8=1 PYTHONIOENCODING=utf-8 "+
		"AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED "+
		"AWS_RETRY_MODE=standard AWS_MAX_ATTEMPTS=10 AWS_PROFILE=wasabi" {
		t.Errorf("environnement inattendu : %v", env)
	}
}

func TestClassify(t *testing.T) {
	cases := map[int]Outcome{0: OK, 2: Warning, 1: Retryable, 254: Retryable, 255: Retryable, 252: Failed, 253: Failed, 130: Failed}
	for code, want := range cases {
		if got := Classify(code); got != want {
			t.Errorf("Classify(%d) = %v, attendu %v", code, got, want)
		}
	}
}

func TestCounterCountsAcrossChunks(t *testing.T) {
	var sink strings.Builder
	c := NewCounter(&sink)
	chunks := []string{
		"upload: E:\\a.txt to s3://acme-srv01/DATA/a.txt\nupl",
		"oad: E:\\b.txt to s3://acme-srv01/DATA/b.txt\n(dryrun) delete: s3://acme-srv01/DATA/old.txt\n",
		"warning: Skipping file E:\\locked.pst. File/Directory is not readable.\n",
		"delete: s3://acme-srv01/DATA/x.txt", // dernière ligne sans saut de ligne
	}
	for _, ch := range chunks {
		c.Write([]byte(ch))
	}
	got := c.Counts()
	if !reflect.DeepEqual(got, Counts{Uploaded: 2, Deleted: 2, Skipped: 1}) {
		t.Errorf("comptes = %+v", got)
	}
	if sink.String() != strings.Join(chunks, "") {
		t.Error("la sortie doit être recopiée telle quelle dans le journal")
	}
}

// Le filtrage reproduit celui de la CLI (motif joint au dossier source, fnmatch).
func TestExcludeMatcher(t *testing.T) {
	m := excludeMatcher("D:/Projets", []string{"*/.git/*", "*/.venv/*", "*.pyc", "*Thumbs.db", "build[0-9]/*"}, true)
	for path, want := range map[string]bool{
		`D:\Projets\outils\agent\.venv\lib64`:              true,
		`D:\Projets\outils\agent\.venv\bin\\U0001d70bthon`: true,
		`d:\projets\x\.GIT\config`:                         true, // casse indifférente
		`D:\Projets\a\b\c.pyc`:                             true,
		`D:\Projets\photos\Thumbs.db`:                      true,
		`D:\Projets\build7\out.bin`:                        true,
		`D:\Projets\buildx\out.bin`:                        false,
		`D:\Projets\.venv-notes.txt`:                       false,
		`D:\Projets\src\main.go`:                           false,
		`E:\Autre\.venv\lib64`:                             false, // hors de la source
	} {
		if got := m(path); got != want {
			t.Errorf("%s : %v, attendu %v", path, got, want)
		}
	}
}

func TestSkippedExcludedAreSeparated(t *testing.T) {
	c := NewCounter(nil)
	c.Excluded = excludeMatcher("D:/Projets", []string{"*/.venv/*"}, true)
	c.Write([]byte("warning: Skipping file D:\\Projets\\p\\.venv\\lib64. File does not exist.\n" +
		"warning: Skipping file D:\\Projets\\p\\notes. version 2.docx. File/Directory is not readable.\n"))
	if got := c.Counts(); got.Skipped != 1 || got.SkippedExcluded != 1 {
		t.Fatalf("comptes : %+v", got)
	}
}

// Hors Windows : chemins Unix, casse respectée.
func TestExcludeMatcherUnix(t *testing.T) {
	m := excludeMatcher("/home/alice/Documents/", []string{"*/.git/*", "*.tmp"}, false)
	for path, want := range map[string]bool{
		"/home/alice/Documents/projet/.git/config": true,
		"/home/alice/Documents/a.tmp":              true,
		"/home/alice/Documents/projet/.GIT/config": false, // casse respectée
		"/home/alice/Documents/A.TMP":              false,
		"/home/bob/Documents/a.tmp":                false, // hors de la source
	} {
		if got := m(path); got != want {
			t.Errorf("%s : %v, attendu %v", path, got, want)
		}
	}
}

// Chaque fichier envoyé est contrôlé : un lien apparu en cours de route est relevé une fois.
func TestCounterLinked(t *testing.T) {
	c := NewCounter(nil)
	var seen []string
	c.Linked = func(p string) (string, bool) {
		seen = append(seen, p)
		if strings.Contains(p, `\j\`) {
			return "Outlook/j", true
		}
		return "", false
	}
	c.Write([]byte("upload: C:\\Users\\alice\\Outlook\\a.pst to s3://b/k/a.pst\n" +
		"upload: C:\\Users\\alice\\Outlook\\j\\x.secret to s3://b/k/j/x.secret\n" +
		"upload: C:\\Users\\alice\\Outlook\\j\\y.secret to s3://b/k/j/y.secret\n"))
	got := c.Counts()
	if got.Uploaded != 3 || len(got.Linked) != 1 || got.Linked[0] != "Outlook/j" {
		t.Fatalf("%+v", got)
	}
	if seen[0] != `C:\Users\alice\Outlook\a.pst` {
		t.Fatalf("chemin extrait : %q", seen[0])
	}
}

// Les motifs d'exclusion d'un lien relevé écartent le lien et tout ce qui passe par lui.
func TestLinkExcludePatternsMatch(t *testing.T) {
	m := excludeMatcher(`C:\Users\alice\Outlook`, []string{"j [[]1]", "j [[]1]/*"}, true)
	for _, p := range []string{`C:\Users\alice\Outlook\j [1]`, `C:\Users\alice\Outlook\j [1]\wasabi.secret`, `c:\users\alice\outlook\J [1]\a\b`} {
		if !m(p) {
			t.Errorf("%s non exclu", p)
		}
	}
	if m(`C:\Users\alice\Outlook\j1\a`) || m(`C:\Users\alice\Outlook\alice.pst`) {
		t.Error("exclusion trop large")
	}
}
