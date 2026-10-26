// Package awsinstall installe la CLI AWS v2 sur un poste qui ne l'a pas encore, pour
// `kura-agent install` : MSI fourni avec le paquet d'installation s'il est là, sinon
// téléchargé chez AWS. Dans les deux cas, la signature Authenticode du MSI est
// vérifiée (valide, signataire Amazon) avant de lancer msiexec.
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
)

// DownloadURL : dernière version de la CLI AWS v2 pour Windows (64 bits).
const DownloadURL = "https://awscli.amazonaws.com/AWSCLIV2.msi"

// MSIName : nom attendu du MSI posé à côté du paquet ou de kura-agent.exe.
const MSIName = "AWSCLIV2.msi"

// maxDownload : le MSI fait environ 40 Mo ; au-delà de 300 Mo, ce n'est pas lui.
const maxDownload = 300 << 20

// Signers : organisations (champ O= du certificat) acceptées comme signataire du MSI.
var Signers = []string{"Amazon.com Services LLC", "Amazon Web Services, Inc.", "Amazon.com, Inc."}

// ErrUnsupported : vérification de signature et msiexec n'existent que sous Windows.
var ErrUnsupported = errors.New("installation de la CLI AWS : Windows uniquement")

// Installer regroupe les opérations, remplaçables dans les tests.
type Installer struct {
	// Find cherche aws.exe (PATH, puis emplacement d'installation par défaut).
	Find func() (string, error)
	// Signature renvoie l'état Authenticode (« Valid »…) et le sujet du signataire.
	Signature func(msi string) (status, subject string, err error)
	Download  func(url, dst string) error
	// RunMSI lance l'installation silencieuse ; rebootRequired pour le code 3010.
	RunMSI  func(msi, logFile string) (rebootRequired bool, err error)
	Version func(aws string) (string, error)
	TempDir string
	Out     io.Writer
}

// Result décrit la CLI trouvée ou installée.
type Result struct {
	Path, Version  string
	Installed      bool
	Source         string // MSI utilisé (chemin local, ou URL de téléchargement)
	Signer         string
	RebootRequired bool
}

// Ensure vérifie que la CLI AWS est présente et l'installe sinon. candidates :
// emplacements possibles d'un MSI fourni, dans l'ordre de préférence ; le premier
// qui existe est utilisé, sinon le MSI est téléchargé.
func (in Installer) Ensure(candidates []string) (Result, error) {
	if p, err := in.Find(); err == nil {
		v, err := in.Version(p)
		if err != nil {
			return Result{}, fmt.Errorf("%s ne répond pas (%v) : réparer ou réinstaller la CLI AWS", p, err)
		}
		return Result{Path: p, Version: v}, nil
	}
	res := Result{Installed: true}
	msi := ""
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			msi = c
			break
		}
	}
	if msi == "" {
		if err := os.MkdirAll(in.TempDir, 0o700); err != nil {
			return Result{}, err
		}
		msi = filepath.Join(in.TempDir, MSIName)
		fmt.Fprintf(in.Out, "  CLI AWS absente : téléchargement de %s…\n", DownloadURL)
		if err := in.Download(DownloadURL, msi); err != nil {
			_ = os.Remove(msi)
			return Result{}, fmt.Errorf("téléchargement de la CLI AWS : %w (poser %s à côté du paquet et relancer)", err, MSIName)
		}
		defer os.Remove(msi)
		res.Source = DownloadURL
	} else {
		fmt.Fprintf(in.Out, "  CLI AWS absente : installation depuis %s\n", msi)
		res.Source = msi
	}

	status, subject, err := in.Signature(msi)
	if err != nil {
		return Result{}, fmt.Errorf("vérification de la signature de %s : %w", msi, err)
	}
	signer, err := CheckSigner(status, subject)
	if err != nil {
		return Result{}, fmt.Errorf("%s refusé, rien n'a été installé : %w", msi, err)
	}
	res.Signer = signer

	logFile := filepath.Join(in.TempDir, "awscli-install.log")
	if err := os.MkdirAll(in.TempDir, 0o700); err != nil {
		return Result{}, err
	}
	reboot, err := in.RunMSI(msi, logFile)
	if err != nil {
		return Result{}, fmt.Errorf("installation de la CLI AWS : %w (journal : %s)", err, logFile)
	}
	res.RebootRequired = reboot
	p, err := in.Find()
	if err != nil {
		return Result{}, fmt.Errorf("CLI AWS installée mais introuvable (%v) ; journal : %s", err, logFile)
	}
	v, err := in.Version(p)
	if err != nil {
		return Result{}, fmt.Errorf("%s installé mais ne répond pas : %v", p, err)
	}
	res.Path, res.Version = p, v
	return res, nil
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
	client := &http.Client{Timeout: 20 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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

// DefaultPath : emplacement d'installation du MSI (C:\Program Files\Amazon\AWSCLIV2).
// Le PATH du processus qui vient de lancer msiexec n'est pas mis à jour : la CLI
// fraîchement installée est cherchée là.
func DefaultPath() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		return ""
	}
	return filepath.Join(pf, "Amazon", "AWSCLIV2", "aws.exe")
}
