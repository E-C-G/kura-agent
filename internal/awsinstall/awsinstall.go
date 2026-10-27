// Package awsinstall installe la CLI AWS v2 sur un poste qui ne l'a pas encore, pour
// `kura-agent install` : paquet fourni avec le paquet d'installation s'il est là,
// sinon téléchargé chez AWS. Dans les deux cas, la signature est vérifiée avant
// l'installation :
//
//	Windows   AWSCLIV2.msi                 Authenticode valide, signataire Amazon, msiexec
//	Linux     awscli-exe-linux-<arch>.zip  signature PGP (.sig) de la clé « AWS CLI Team »
//	          et sa signature .sig         intégrée à l'agent (gpgv), puis aws/install
package awsinstall

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/platform"
)

// DownloadURL : dernière version de la CLI AWS v2 pour Windows (64 bits).
const DownloadURL = "https://awscli.amazonaws.com/AWSCLIV2.msi"

// MSIName : nom attendu du MSI posé à côté du paquet ou de kura-agent.exe.
const MSIName = "AWSCLIV2.msi"

// maxDownload : le MSI fait environ 40 Mo, le zip Linux environ 60 Mo ; au-delà de
// 300 Mo, ce n'est pas lui.
const maxDownload = 300 << 20

// Signers : organisations (champ O= du certificat) acceptées comme signataire du MSI.
var Signers = []string{"Amazon.com Services LLC", "Amazon Web Services, Inc.", "Amazon.com, Inc."}

// ErrUnsupported : installation automatique pas encore écrite pour ce système
// (errors.Is ; le détail est une platform.NotImplementedError).
var ErrUnsupported = platform.ErrNotImplemented

// Package décrit la CLI AWS telle qu'AWS la distribue pour un système.
type Package struct {
	Name   string // fichier, posé à côté du paquet .kura ou téléchargé
	URL    string
	SigURL string // signature détachée (Linux : <URL>.sig), vide si elle est intégrée
	// Verify contrôle le fichier (et sa signature) et renvoie le signataire.
	Verify func(file, sig string) (signer string, err error)
	// Install installe depuis le fichier vérifié ; work : dossier de travail privé.
	Install func(file, work, logFile string) (rebootRequired bool, err error)
}

// SigName : nom de la signature détachée, posée à côté du fichier.
func (p Package) SigName() string {
	if p.SigURL == "" {
		return ""
	}
	return p.Name + ".sig"
}

// Installer regroupe les opérations, remplaçables dans les tests.
type Installer struct {
	// Find cherche aws (PATH, puis emplacement d'installation par défaut).
	Find     func() (string, error)
	Download func(url, dst string) error
	Version  func(aws string) (string, error)
	Package  Package
	TempDir  string
	Out      io.Writer
}

// Result décrit la CLI trouvée ou installée.
type Result struct {
	Path, Version  string
	Installed      bool
	Source         string // paquet utilisé (chemin local, ou URL de téléchargement)
	Signer         string
	RebootRequired bool
	// Warning : CLI présente mais qui ne répond pas ; l'installation continue,
	// validate dira si elle fonctionne.
	Warning string
}

// Ensure vérifie que la CLI AWS v2 est présente et l'installe sinon.
//
// want : chemin exigé de aws (profile.aws_cli), vide pour le PATH ou l'emplacement
// par défaut. candidates : emplacements possibles d'un paquet fourni (Package.Name),
// dans l'ordre de préférence ; le premier qui existe est utilisé (avec sa signature
// .sig à côté, quand le système en a une), sinon le paquet est téléchargé.
//
// Le paquet est toujours copié (ou téléchargé) dans TempDir, qui doit être un dossier
// que seuls les administrateurs peuvent modifier : la signature vérifiée est bien
// celle du fichier installé, sans qu'un processus non élevé puisse le remplacer
// entre les deux.
func (in Installer) Ensure(want string, candidates []string) (Result, error) {
	pkg := in.Package
	find := func() (string, error) {
		if want == "" {
			return in.Find()
		}
		if st, err := os.Stat(want); err != nil || !st.Mode().IsRegular() {
			return "", fmt.Errorf("%s introuvable", want)
		}
		return want, nil
	}
	if p, err := find(); err == nil {
		v, err := in.Version(p)
		if err != nil {
			return Result{Path: p, Warning: fmt.Sprintf("%s ne répond pas (%v)", p, err)}, nil
		}
		if !strings.HasPrefix(v, "aws-cli/2.") {
			return Result{}, fmt.Errorf("%s est la CLI AWS %s : la v2 est nécessaire (désinstaller l'ancienne, ou renseigner profile.aws_cli)", p, v)
		}
		if RecentEnough(v) {
			return Result{Path: p, Version: v}, nil
		}
		if pkg.Name == "" || pkg.Verify == nil || pkg.Install == nil {
			return Result{}, fmt.Errorf("%s : CLI AWS %s antérieure à %s, à mettre à jour", p, cliVersion(v), MinCLIVersion)
		}
		fmt.Fprintf(in.Out, "  CLI AWS %s antérieure à %s : mise à jour\n", cliVersion(v), MinCLIVersion)
	}
	if pkg.Name == "" || pkg.Verify == nil || pkg.Install == nil {
		return Result{}, platform.NotImplemented(platform.AWSCLISetup)
	}
	res := Result{Installed: true}
	if err := os.RemoveAll(in.TempDir); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(in.TempDir, 0o700); err != nil {
		return Result{}, err
	}
	file := filepath.Join(in.TempDir, pkg.Name)
	sig := ""
	if pkg.SigName() != "" {
		sig = filepath.Join(in.TempDir, pkg.SigName())
		defer os.Remove(sig)
	}
	defer os.Remove(file)
	src := ""
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			src = c
			break
		}
	}
	if src == "" {
		fmt.Fprintf(in.Out, "  CLI AWS absente : téléchargement de %s…\n", pkg.URL)
		err := in.Download(pkg.URL, file)
		if err == nil && sig != "" {
			err = in.Download(pkg.SigURL, sig)
		}
		if err != nil {
			return Result{}, fmt.Errorf("téléchargement de la CLI AWS : %w (poser %s à côté du paquet et relancer)", err, pkg.Name)
		}
		res.Source = pkg.URL
	} else {
		fmt.Fprintf(in.Out, "  CLI AWS absente : installation depuis %s\n", src)
		if err := copyFile(src, file); err != nil {
			return Result{}, fmt.Errorf("copie de %s : %w", src, err)
		}
		if sig != "" {
			if err := copyFile(src+".sig", sig); err != nil {
				return Result{}, fmt.Errorf("signature %s.sig : %w (elle accompagne toujours %s)", src, err, pkg.Name)
			}
		}
		res.Source = src
	}

	signer, err := pkg.Verify(file, sig)
	if err != nil {
		return Result{}, fmt.Errorf("%s refusé, rien n'a été installé : %w", res.Source, err)
	}
	res.Signer = signer

	logFile := filepath.Join(in.TempDir, "awscli-install.log")
	reboot, err := pkg.Install(file, in.TempDir, logFile)
	if err != nil {
		return Result{}, fmt.Errorf("installation de la CLI AWS : %w (journal : %s)", err, logFile)
	}
	res.RebootRequired = reboot
	p, err := find()
	if err != nil {
		return Result{}, fmt.Errorf("CLI AWS installée mais introuvable (%v) ; journal : %s", err, logFile)
	}
	v, err := in.Version(p)
	if err != nil {
		return Result{}, fmt.Errorf("%s installé mais ne répond pas : %v", p, err)
	}
	if !RecentEnough(v) {
		// Paquet authentique mais ancien (posé à côté du paquet .kura) : refusé.
		return Result{}, fmt.Errorf("%s a installé la CLI AWS %s, antérieure à %s : paquet trop ancien, retirer %s et relancer (téléchargement de la dernière version)",
			res.Source, cliVersion(v), MinCLIVersion, pkg.Name)
	}
	res.Path, res.Version = p, v
	return res, nil
}

// MinCLIVersion : plus ancienne CLI AWS v2 acceptée. Un paquet signé par Amazon mais
// ancien (vulnérabilités connues) est refusé, une CLI installée plus ancienne est mise
// à jour. À relever régulièrement, avec la clé PGP d'AWS.
const MinCLIVersion = "2.32.0"

// RecentEnough : « aws-cli/2.x.y Python/… » au moins MinCLIVersion.
func RecentEnough(versionLine string) bool {
	return versionAtLeast(cliVersion(versionLine), MinCLIVersion)
}

// cliVersion extrait « 2.x.y » de « aws-cli/2.x.y Python/… ».
func cliVersion(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return strings.TrimPrefix(f[0], "aws-cli/")
}

func versionAtLeast(v, min string) bool {
	parse := func(s string) []int {
		var out []int
		for _, p := range strings.Split(s, ".") {
			n := 0
			if p == "" {
				return nil
			}
			for _, c := range p {
				if c < '0' || c > '9' {
					return nil
				}
				n = n*10 + int(c-'0')
			}
			out = append(out, n)
		}
		return out
	}
	a, b := parse(v), parse(min)
	if a == nil || b == nil {
		return false
	}
	for i := 0; i < len(b); i++ {
		x := 0
		if i < len(a) {
			x = a[i]
		}
		if x != b[i] {
			return x > b[i]
		}
	}
	return true
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(in, maxDownload+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// verifyMSI : signature Authenticode valide d'une organisation Amazon.
func verifyMSI(signature func(string) (string, string, error)) func(file, sig string) (string, error) {
	return func(file, _ string) (string, error) {
		status, subject, err := signature(file)
		if err != nil {
			return "", fmt.Errorf("vérification de la signature : %w", err)
		}
		return CheckSigner(status, subject)
	}
}

// CheckSigner accepte une signature valide d'une organisation de Signers et renvoie
// son nom.
func CheckSigner(status, subject string) (string, error) {
	if status != "Valid" {
		if status == "" {
			status = "absente"
		}
		return "", fmt.Errorf("signature %s", status)
	}
	var orgs []string
	for _, rdn := range splitDN(subject) {
		if k, v, ok := strings.Cut(rdn, "="); ok && strings.TrimSpace(k) == "O" {
			orgs = append(orgs, unquote(strings.TrimSpace(v)))
		}
	}
	if len(orgs) != 1 {
		return "", fmt.Errorf("signataire sans organisation unique : %q", subject)
	}
	org := orgs[0]
	for _, s := range Signers {
		if org == s {
			return org, nil
		}
	}
	return "", fmt.Errorf("signé par %q, pas par Amazon", org)
}

// splitDN découpe un sujet X.500 tel que l'affiche .NET (« CN=…, O="…, Inc.", C=US »)
// aux virgules hors guillemets.
func splitDN(dn string) []string {
	var parts []string
	var b strings.Builder
	quoted := false
	for _, r := range dn {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			parts = append(parts, strings.TrimSpace(b.String()))
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	return append(parts, strings.TrimSpace(b.String()))
}

func unquote(v string) string {
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		return strings.ReplaceAll(v[1:len(v)-1], `""`, `"`)
	}
	return v
}

// HTTPDownload télécharge url dans dst (HTTPS uniquement, 300 Mo au plus).
func HTTPDownload(url, dst string) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("adresse non HTTPS refusée : %s", url)
	}
	client := &http.Client{Timeout: 20 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" {
			return fmt.Errorf("redirection non HTTPS refusée : %s", r.URL)
		}
		if len(via) >= 10 {
			return errors.New("trop de redirections")
		}
		return nil
	}}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxDownload {
		err = errors.New("fichier anormalement gros")
	}
	return err
}
