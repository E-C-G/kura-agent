// Package awscli construit les commandes `aws s3 sync` et interprète leur sortie.
package awscli

import (
	"bufio"
	"bytes"
	"io"
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
	}
	if p.Region != "" {
		args = append(args, "--region", p.Region)
	}
	if p.ChecksumAlgorithm != "" {
		args = append(args, "--checksum-algorithm", p.ChecksumAlgorithm)
	}
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
	Skipped  int // lignes "warning: Skipping file …"
}

// Counter compte les opérations au fil de la sortie et la recopie dans un io.Writer.
// Il est sûr pour un usage concurrent (stdout et stderr écrits en parallèle).
type Counter struct {
	mu     sync.Mutex
	out    io.Writer
	buf    bytes.Buffer
	counts Counts
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
		c.counts.Skipped++
	}
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
