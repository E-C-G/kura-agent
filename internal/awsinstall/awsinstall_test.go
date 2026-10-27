package awsinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckSigner(t *testing.T) {
	ok := []struct{ subject, org string }{
		{"CN=Amazon.com Services LLC, O=Amazon.com Services LLC, L=Seattle, S=Washington, C=US", "Amazon.com Services LLC"},
		{`CN="Amazon Web Services, Inc.", O="Amazon Web Services, Inc.", L=Seattle, S=Washington, C=US`, "Amazon Web Services, Inc."},
		{`O="Amazon.com, Inc.", C=US`, "Amazon.com, Inc."},
	}
	for _, c := range ok {
		got, err := CheckSigner("Valid", c.subject)
		if err != nil || got != c.org {
			t.Errorf("%q : %q, %v", c.subject, got, err)
		}
	}
	bad := []struct{ status, subject string }{
		{"NotSigned", ""},
		{"HashMismatch", "CN=Amazon.com Services LLC, O=Amazon.com Services LLC"},
		{"", ""},
		{"Valid", "CN=Amazon.com Services LLC"}, // pas d'organisation
		{"Valid", "CN=Evil, O=Amazon.com Services LLC Fake, C=US"},      // nom approchant
		{"Valid", "CN=Amazon.com Services LLC, O=Mallory Ltd, C=US"},    // CN trompeur
		{"Valid", `CN=x, O="Amazon.com Services LLC, Evil", C=US`},      // virgule dans O
		{"Valid", `CN="a, O=Amazon.com Services LLC", O=Mallory, C=US`}, // O= dans un CN entre guillemets
		{"Valid", `O=Amazon.com Services LLC, O=Mallory`},               // deux organisations
	}
	for _, c := range bad {
		if org, err := CheckSigner(c.status, c.subject); err == nil {
			t.Errorf("%s %q accepté (%s)", c.status, c.subject, org)
		}
	}
}

type fake struct {
	installed  bool
	downloaded string
	ran        string
	ranContent string
	signed     string // fichier dont la signature a été lue
	status     string
	subject    string
	dlErr      error
	version    string
	versionErr error
}

func (f *fake) installer(dir string) Installer {
	return Installer{
		Find: func() (string, error) {
			if f.installed {
				return `C:\Program Files\Amazon\AWSCLIV2\aws.exe`, nil
			}
			return "", errors.New("absent")
		},
		Package: Package{Name: MSIName, URL: DownloadURL,
			Verify: verifyMSI(func(msi string) (string, string, error) { f.signed = msi; return f.status, f.subject, nil }),
			Install: func(msi, work, log string) (bool, error) {
				f.ran = msi
				b, _ := os.ReadFile(msi)
				f.ranContent = string(b)
				f.installed = true
				return false, nil
			},
		},
		Download: func(url, dst string) error {
			f.downloaded = url
			if f.dlErr != nil {
				return f.dlErr
			}
			return os.WriteFile(dst, []byte("msi"), 0o600)
		},
		Version: func(string) (string, error) {
			if f.version != "" || f.versionErr != nil {
				return f.version, f.versionErr
			}
			return "aws-cli/2.35.0 Python/3.13 Windows/11", nil
		},
		TempDir: filepath.Join(dir, "tmp"),
		Out:     &strings.Builder{},
	}
}

const amazon = "CN=Amazon.com Services LLC, O=Amazon.com Services LLC, L=Seattle, S=Washington, C=US"

func TestEnsureAlreadyInstalled(t *testing.T) {
	f := &fake{installed: true}
	res, err := f.installer(t.TempDir()).Ensure("", nil)
	if err != nil || res.Installed || f.ran != "" || f.downloaded != "" || !strings.HasPrefix(res.Version, "aws-cli/2") {
		t.Fatalf("%+v %v %+v", res, err, f)
	}
}

func TestEnsureUsesProvidedMSI(t *testing.T) {
	dir := t.TempDir()
	msi := filepath.Join(dir, MSIName)
	if err := os.WriteFile(msi, []byte("msi"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fake{status: "Valid", subject: amazon}
	in := f.installer(dir)
	res, err := in.Ensure("", []string{"", filepath.Join(dir, "absent.msi"), msi})
	if err != nil || !res.Installed || f.downloaded != "" || res.Source != msi || res.Signer != "Amazon.com Services LLC" {
		t.Fatalf("%+v %v %+v", res, err, f)
	}
	// Copie privée : la signature vérifiée est celle du fichier installé, dans le
	// dossier de travail protégé, jamais l'original.
	if f.signed != f.ran || filepath.Dir(f.ran) != in.TempDir || f.ranContent != "msi" {
		t.Fatalf("signé %s, installé %s (%q)", f.signed, f.ran, f.ranContent)
	}
	if _, err := os.Stat(f.ran); !os.IsNotExist(err) {
		t.Fatal("copie du MSI laissée")
	}
	if _, err := os.Stat(msi); err != nil {
		t.Fatal("le MSI fourni ne doit pas être supprimé")
	}
}

func TestEnsureDownloadsWhenNoMSI(t *testing.T) {
	dir := t.TempDir()
	f := &fake{status: "Valid", subject: amazon}
	res, err := f.installer(dir).Ensure("", []string{filepath.Join(dir, "absent.msi")})
	if err != nil || f.downloaded != DownloadURL || res.Source != DownloadURL || f.ran == "" {
		t.Fatalf("%+v %v %+v", res, err, f)
	}
	if _, err := os.Stat(f.ran); !os.IsNotExist(err) {
		t.Fatal("le MSI téléchargé doit être supprimé après installation")
	}
}

func TestEnsureRefusesBadSignature(t *testing.T) {
	for _, c := range []struct{ status, subject string }{{"HashMismatch", amazon}, {"Valid", "CN=x, O=Mallory Ltd"}, {"NotSigned", ""}} {
		dir := t.TempDir()
		f := &fake{status: c.status, subject: c.subject}
		_, err := f.installer(dir).Ensure("", nil)
		if err == nil || f.ran != "" {
			t.Fatalf("%s %q : msiexec lancé (%v)", c.status, c.subject, err)
		}
	}
}

func TestEnsureDownloadFailure(t *testing.T) {
	f := &fake{dlErr: errors.New("HTTP 403")}
	_, err := f.installer(t.TempDir()).Ensure("", nil)
	if err == nil || !strings.Contains(err.Error(), MSIName) || f.ran != "" {
		t.Fatalf("%v %+v", err, f)
	}
}

func TestHTTPDownloadRefusesHTTP(t *testing.T) {
	if err := HTTPDownload("http://example.invalid/x.msi", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("http accepté")
	}
}

func TestEnsureWantedPath(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "AWSCLIV2", "aws.exe")
	// Une autre CLI dans le PATH ne compte pas : le chemin configuré manque.
	f := &fake{installed: true, status: "Valid", subject: amazon}
	in := f.installer(dir)
	in.Package.Install = func(msi, work, log string) (bool, error) {
		f.ran = msi
		os.MkdirAll(filepath.Dir(want), 0o755)
		return false, os.WriteFile(want, nil, 0o755)
	}
	res, err := in.Ensure(want, nil)
	if err != nil || !res.Installed || res.Path != want {
		t.Fatalf("%+v %v", res, err)
	}
	// Présent : rien à faire.
	f.ran = ""
	if res, err := in.Ensure(want, nil); err != nil || res.Installed || f.ran != "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestEnsureExistingCLI(t *testing.T) {
	f := &fake{installed: true, version: "aws-cli/1.33.0 Python/3.11 Windows/10"}
	if _, err := f.installer(t.TempDir()).Ensure("", nil); err == nil || !strings.Contains(err.Error(), "v2") {
		t.Fatalf("CLI v1 acceptée : %v", err)
	}
	f = &fake{installed: true, versionErr: errors.New("délai dépassé")}
	res, err := f.installer(t.TempDir()).Ensure("", nil)
	if err != nil || res.Warning == "" || f.ran != "" {
		t.Fatalf("CLI qui ne répond pas : %+v %v", res, err)
	}
}

func TestParseSignatureOutput(t *testing.T) {
	st, sub, err := parseSignatureOutput("\ufeffValid\r\nCN=Amazon.com Services LLC, O=Amazon.com Services LLC\r\n")
	if err != nil || st != "Valid" || !strings.HasPrefix(sub, "CN=Amazon") {
		t.Fatalf("%q %q %v", st, sub, err)
	}
	if _, _, err := parseSignatureOutput("  \r\n"); err == nil {
		t.Fatal("réponse vide acceptée")
	}
}

func TestIsSnapPath(t *testing.T) {
	eval := func(p string) (string, error) {
		return map[string]string{
			"/usr/local/bin/aws": "/usr/local/aws-cli/v2/2.35.21/dist/aws",
			"/bin/aws-snap-lien": "/usr/bin/snap",
		}[p], nil
	}
	for p, want := range map[string]bool{"/snap/bin/aws": true, "/bin/aws-snap-lien": true, "/usr/local/bin/aws": false} {
		if got := isSnapPath(p, eval); got != want {
			t.Errorf("%s : %v", p, got)
		}
	}
}

// Audit M4 : une CLI installée trop ancienne est mise à jour ; un paquet authentique mais
// ancien est refusé après installation.
func TestMinimumCLIVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"aws-cli/2.32.0 Python/3.13": true, "aws-cli/2.35.21 Python/3.13 Linux/6": true, "aws-cli/3.0.0": true,
		"aws-cli/2.31.99 Python/3.13": false, "aws-cli/2.9.0": false, "aws-cli/2": false, "aws-cli/x": false, "": false,
	} {
		if RecentEnough(v) != want {
			t.Errorf("%q : %v", v, !want)
		}
	}
	// Présente mais ancienne : réinstallée.
	dir := t.TempDir()
	f := &fake{installed: true, status: "Valid", subject: amazon, version: "aws-cli/2.20.0 Python/3.11 Windows/10"}
	in := f.installer(dir)
	in.Version = func(string) (string, error) {
		if f.ran != "" {
			return "aws-cli/2.35.0 Python/3.13 Windows/11", nil
		}
		return "aws-cli/2.20.0 Python/3.11 Windows/10", nil
	}
	res, err := in.Ensure("", nil)
	if err != nil || !res.Installed || f.ran == "" || !strings.Contains(in.Out.(*strings.Builder).String(), "mise à jour") {
		t.Fatalf("mise à jour : %+v %v", res, err)
	}
	// Paquet ancien (authentiquement signé) : refusé.
	f = &fake{status: "Valid", subject: amazon, version: "aws-cli/2.20.0 Python/3.11 Windows/10"}
	if _, err := f.installer(t.TempDir()).Ensure("", nil); err == nil || !strings.Contains(err.Error(), "trop ancien") {
		t.Fatalf("paquet ancien accepté : %v", err)
	}
}
