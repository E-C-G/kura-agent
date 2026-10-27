// Package awscli construit les commandes `aws s3 sync` et interprète leur sortie.
package awscli

import (
	"bufio"
	"bytes"
	"io"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/E-C-G/kura-agent/pkg/config"
)

// SyncArgs renvoie les arguments de `aws s3 sync` pour un job.
// dryRun ajoute --dryrun (simulation, utilisée par le garde-fou et par --dry-run).
func SyncArgs(p config.Profile, j config.Job, dryRun bool) []string {
	args := []string{
		"s3", "sync",
		j.Source,
		"s3://" + p.Bucket + "/" + j.Destination,
		"--endpoint-url", p.Endpoint,
		"--no-progress",
		// Liens symboliques (et, selon la CLI, jonctions) non suivis : un utilisateur qui
		// écrit dans une source ne doit pas pouvoir y faire entrer un fichier qu'il ne
		// sait pas lire (secrets de l'agent, profil d'un autre utilisateur, /etc/shadow).
		// Le garde-fou compte de même, sans suivre les liens.
		"--no-follow-symlinks",
	}
	if p.Region != "" {
		args = append(args, "--region", p.Region)
	}
	// profile.checksum_algorithm n'est plus transmis : avec --checksum-algorithm, la CLI
	// v2 envoie la somme de contrôle en fin de requête (trailer), que Wasabi ne lit pas ;
	// les envois en plusieurs parties (fichiers de plus de 8 Mo) échouaient en
	// « Checksum Type mismatch … expected crc32, actual null ».
	for _, e := range j.Exclude {
		args = append(args, "--exclude", e)
	}
	if j.Delete {
		args = append(args, "--delete")
	}
	if dryRun {
		args = append(args, "--dryrun")
	}
	return args
}

// Env renvoie les variables d'environnement à passer à aws.exe.
func Env(p config.Profile) []string {
	env := []string{
		"AWS_PAGER=", // jamais de pager : aws.exe n'a pas de console
		// Sortie en UTF-8 : sinon, redirigée vers un tube, la CLI (Python) écrit dans la page
		// de code ANSI et peut perdre les lignes des fichiers aux noms non latins, ce qui
		// fausserait le nombre de suppressions vu par le garde-fou.
		"PYTHONUTF8=1",
		"PYTHONIOENCODING=utf-8",
		// Depuis la CLI 2.23 (janvier 2025), sommes de contrôle CRC par défaut, envoyées en
		// fin de requête : Wasabi ne les prend pas en charge. Seulement quand l'opération
		// l'exige, comme avant ; l'intégrité du transfert reste assurée par TLS.
		"AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED",
		"AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED",
	}
	if p.AWSProfile != "" {
		env = append(env, "AWS_PROFILE="+p.AWSProfile)
	}
	return env
}

// Outcome classe un code de sortie de la CLI AWS.
type Outcome int

const (
	OK        Outcome = iota
	Warning           // code 2 : des fichiers ont été ignorés, le reste est transféré
	Retryable         // erreur susceptible de passer à la tentative suivante
	Failed            // erreur définitive
)

// Classify interprète le code de sortie de `aws s3 sync`.
//
//	0          tout est transféré
//	1          au moins un transfert a échoué (souvent réseau)  -> nouvel essai
//	2          des fichiers ont été ignorés (verrouillés…)        -> avertissement
//	252, 253   syntaxe ou configuration                           -> échec définitif
//	254, 255   erreur du service ou erreur générale               -> nouvel essai
func Classify(code int) Outcome {
	switch code {
	case 0:
		return OK
	case 2:
		return Warning
	case 1, 254, 255:
		return Retryable
	default:
		return Failed
	}
}

// Counts résume une sortie de `aws s3 sync`.
type Counts struct {
	Uploaded int
	Deleted  int
	Skipped  int // lignes "warning: Skipping file …" (hors fichiers exclus)
	// SkippedExcluded : fichiers signalés par la CLI alors qu'une exclusion du job les
	// écarte de toute façon. La CLI vérifie l'existence de chaque fichier avant
	// d'appliquer --exclude : un lien symbolique cassé dans un dossier exclu (.venv
	// créé sous WSL…) sort en « File does not exist » et en code 2 à chaque passage.
	SkippedExcluded int
}

// Counter compte les opérations au fil de la sortie et la recopie dans un io.Writer.
// Il est sûr pour un usage concurrent (stdout et stderr écrits en parallèle).
type Counter struct {
	mu     sync.Mutex
	out    io.Writer
	buf    bytes.Buffer
	counts Counts
	// Excluded dit si un chemin local est écarté par les exclusions du job (nil : aucun).
	Excluded func(path string) bool
}

// NewCounter recopie tout ce qu'il reçoit dans out (le journal du job).
func NewCounter(out io.Writer) *Counter { return &Counter{out: out} }

func (c *Counter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.out != nil {
		if _, err := c.out.Write(p); err != nil {
			return 0, err
		}
	}
	c.buf.Write(p)
	for {
		i := bytes.IndexByte(c.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := string(c.buf.Next(i + 1))
		c.countLine(line)
	}
	return len(p), nil
}

// Counts renvoie les totaux, ligne incomplète finale comprise.
func (c *Counter) Counts() Counts {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buf.Len() > 0 {
		c.countLine(c.buf.String())
		c.buf.Reset()
	}
	return c.counts
}

func (c *Counter) countLine(line string) {
	l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "(dryrun)"))
	switch {
	case strings.HasPrefix(l, "upload:"):
		c.counts.Uploaded++
	case strings.HasPrefix(l, "delete:"):
		c.counts.Deleted++
	case strings.HasPrefix(strings.ToLower(l), "warning: skipping file"):
		if c.Excluded != nil && c.Excluded(skippedPath(l)) {
			c.counts.SkippedExcluded++
		} else {
			c.counts.Skipped++
		}
	}
}

// skippedPath extrait le chemin de « warning: Skipping file <chemin>. <raison>. ».
func skippedPath(l string) string {
	rest := strings.TrimSpace(l[len("warning: skipping file"):])
	if i := strings.LastIndex(rest, ". "); i >= 0 {
		return rest[:i]
	}
	return rest
}

// ExcludeMatcher reproduit le filtrage --exclude de la CLI AWS pour une source locale :
// chaque motif est joint au dossier source, puis comparé au chemin complet du fichier
// à la manière de fnmatch (« * » traverse les dossiers, « ? » un caractère, [...]
// une classe). Sous Windows, sans tenir compte de la casse ni du sens des barres ;
// ailleurs, chemins Unix tels quels.
func ExcludeMatcher(source string, patterns []string) func(string) bool {
	return excludeMatcher(source, patterns, runtime.GOOS == "windows")
}

func excludeMatcher(source string, patterns []string, windows bool) func(string) bool {
	sep, norm := "/", func(s string) string { return s }
	if windows {
		sep = `\`
		norm = func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "/", `\`)) }
	}
	root := strings.TrimRight(norm(source), sep)
	var res []*regexp.Regexp
	for _, p := range patterns {
		if re, err := regexp.Compile("^" + fnmatchToRegexp(root+sep+norm(p)) + "$"); err == nil {
			res = append(res, re)
		}
	}
	return func(path string) bool {
		p := norm(path)
		for _, re := range res {
			if re.MatchString(p) {
				return true
			}
		}
		return false
	}
}

// fnmatchToRegexp traduit un motif fnmatch (Python) en expression régulière.
func fnmatchToRegexp(pat string) string {
	var b strings.Builder
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			b.WriteString("(?s:.*)")
		case '?':
			b.WriteString("(?s:.)")
		case '[':
			j := i + 1
			if j < len(pat) && pat[j] == '!' {
				j++
			}
			if j < len(pat) && pat[j] == ']' {
				j++
			}
			for j < len(pat) && pat[j] != ']' {
				j++
			}
			if j >= len(pat) {
				b.WriteString(`\[`)
				continue
			}
			class := pat[i+1 : j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// CountLines compte les opérations d'une sortie complète (tests, relecture de journal).
func CountLines(r io.Reader) Counts {
	c := NewCounter(nil)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		c.countLine(sc.Text())
	}
	return c.counts
}
