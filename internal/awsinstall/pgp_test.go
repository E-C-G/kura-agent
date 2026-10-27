package awsinstall

import (
	"archive/zip"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// La clé intégrée est bien celle dont AWS publie l'empreinte.
func TestAWSCLIKeyFingerprint(t *testing.T) {
	bin, err := dearmor(awsCLIKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := keyFingerprint(bin); err != nil || got != AWSCLIKeyFingerprint {
		t.Fatalf("empreinte %s, %v ; publiée : %s", got, err, AWSCLIKeyFingerprint)
	}
	// Un caractère changé dans la clé change l'empreinte.
	altered := strings.Replace(awsCLIKey, "mQINBF2Cr7UB", "mQINBF2Cr7UC", 1)
	if b, err := dearmor(altered); err == nil {
		if got, _ := keyFingerprint(b); got == AWSCLIKeyFingerprint {
			t.Fatal("clé altérée acceptée")
		}
	}
}

func testKey(t *testing.T) (asc, fpr string) {
	t.Helper()
	a, err := os.ReadFile("testdata/test-key.asc")
	f, err2 := os.ReadFile("testdata/test-key.fpr")
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	return string(a), strings.TrimSpace(string(f))
}

func TestFixtureKeyFingerprint(t *testing.T) {
	asc, fpr := testKey(t)
	bin, err := dearmor(asc)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := keyFingerprint(bin); err != nil || got != fpr {
		t.Fatalf("%s %v, attendu %s", got, err, fpr)
	}
}

// needGPGV : sans gpgv, les vérifications réelles sont sautées, sauf en CI Linux.
func needGPGV(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("vérification gpgv : Linux seulement")
	}
	if _, err := exec.LookPath("gpgv"); err != nil {
		if os.Getenv("CI") != "" && runtime.GOOS == "linux" {
			t.Fatal("gpgv absent de la CI Linux")
		}
		t.Skip("gpgv absent")
	}
}

// copyFixture copie un fichier de testdata dans un dossier de travail.
func copyFixture(t *testing.T, dir, name, as string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, as)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerifyPGPWithGPGV(t *testing.T) {
	needGPGV(t)
	asc, fpr := testKey(t)
	dir := t.TempDir()
	file := copyFixture(t, dir, "payload.zip", "payload.zip")
	sig := copyFixture(t, dir, "payload.zip.sig", "payload.zip.sig")
	if signer, err := verifyPGP(file, sig, asc, fpr, runGPGV); err != nil || !strings.Contains(signer, fpr[:4]) {
		t.Fatalf("%q %v", signer, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "aws-cli-key.gpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("trousseau temporaire laissé")
	}
	// Signé par une autre clé : refusé.
	other := copyFixture(t, dir, "payload.zip.other.sig", "other.sig")
	if _, err := verifyPGP(file, other, asc, fpr, runGPGV); err == nil {
		t.Fatal("signature d'une autre clé acceptée")
	}
	// Fichier modifié : refusé.
	os.WriteFile(file, []byte("archive modifiée\n"), 0o600)
	if _, err := verifyPGP(file, sig, asc, fpr, runGPGV); err == nil {
		t.Fatal("fichier modifié accepté")
	}
	// Bonne signature, mais clé dont l'empreinte n'est pas celle attendue : refusé.
	file = copyFixture(t, dir, "payload.zip", "payload.zip")
	if _, err := verifyPGP(file, sig, asc, AWSCLIKeyFingerprint, runGPGV); err == nil {
		t.Fatal("empreinte inattendue acceptée")
	}
	if _, err := verifyPGP(file, "", asc, fpr, runGPGV); err == nil {
		t.Fatal("signature absente acceptée")
	}
}

func TestVerifyPGPStatusLines(t *testing.T) {
	asc, fpr := testKey(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "f.zip")
	os.WriteFile(file, []byte("x"), 0o600)
	for status, ok := range map[string]bool{
		"[GNUPG:] GOODSIG D8F6 x\n[GNUPG:] VALIDSIG " + fpr + " 2026-10-27 0 0 4 0 1 10 00 " + fpr + "\n": true,
		"[GNUPG:] GOODSIG D8F6 x\n":                                                   false, // sans VALIDSIG
		"[GNUPG:] EXPKEYSIG D8F6 x\n[GNUPG:] VALIDSIG " + fpr + "\n":                  false,
		"[GNUPG:] BADSIG D8F6 x\n":                                                    false,
		"[GNUPG:] GOODSIG D8F6 x\n[GNUPG:] VALIDSIG 0000 2026 0 0 4 0 1 10 00 0000\n": false,
	} {
		_, err := verifyPGP(file, file+".sig", asc, fpr, func(string, string, string) (string, error) { return status, nil })
		if (err == nil) != ok {
			t.Errorf("%q : %v", status, err)
		}
	}
}

func writeZip(t *testing.T, path string, entries map[string]string, symlinks map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range entries {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "install") || strings.HasSuffix(name, "/aws") {
			mode = 0o755
		}
		h.SetMode(mode)
		w, _ := zw.CreateHeader(h)
		w.Write([]byte(content))
	}
	for name, target := range symlinks {
		h := &zip.FileHeader{Name: name}
		h.SetMode(os.ModeSymlink | 0o777)
		w, _ := zw.CreateHeader(h)
		w.Write([]byte(target))
	}
	zw.Close()
	f.Close()
}

func TestExtractZip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("archive Linux (liens symboliques)")
	}
	dir := t.TempDir()
	z := filepath.Join(dir, "a.zip")
	writeZip(t, z, map[string]string{"aws/install": "#!/bin/sh\n", "aws/dist/aws": "bin", "aws/README.md": "doc"},
		map[string]string{"aws/dist/current": "aws"})
	dest := filepath.Join(dir, "out")
	if err := extractZip(z, dest); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dest, "aws", "install"))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("script : %v %v", st, err)
	}
	if l, err := os.Readlink(filepath.Join(dest, "aws", "dist", "current")); err != nil || l != "aws" {
		t.Fatalf("lien : %q %v", l, err)
	}
	for name, c := range map[string]struct {
		entries, links map[string]string
	}{
		"remontée":      {map[string]string{"../evil": "x"}, nil},
		"lien qui sort": {map[string]string{"aws/install": "x"}, map[string]string{"aws/l": "../../etc/passwd"}},
		"lien absolu":   {map[string]string{"aws/install": "x"}, map[string]string{"aws/l": "/etc/passwd"}},
	} {
		z := filepath.Join(dir, "bad.zip")
		writeZip(t, z, c.entries, c.links)
		if err := extractZip(z, filepath.Join(t.TempDir(), "out")); err == nil {
			t.Errorf("%s : accepté", name)
		}
	}
}

func TestInstallZip(t *testing.T) {
	dir := t.TempDir()
	z := filepath.Join(dir, "awscli.zip")
	writeZip(t, z, map[string]string{"aws/install": "#!/bin/sh\n"}, nil)
	var got []string
	run := func(log, name string, args ...string) error {
		got = append([]string{filepath.Base(filepath.Dir(name)) + "/" + filepath.Base(name)}, args...)
		return nil
	}
	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0o700)
	if _, err := installZip(run)(z, work, filepath.Join(work, "log")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "aws/install --install-dir /usr/local/aws-cli --bin-dir /usr/local/bin --update" {
		t.Fatalf("%q", got)
	}
	if _, err := os.Stat(filepath.Join(work, "unpacked")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("archive décompressée laissée")
	}
	writeZip(t, z, map[string]string{"autre/fichier": "x"}, nil)
	if _, err := installZip(run)(z, work, filepath.Join(work, "log")); err == nil {
		t.Fatal("archive sans aws/install acceptée")
	}
}

// Linux : le paquet fourni doit avoir sa signature à côté ; téléchargés, les deux le sont.
func TestEnsureLinuxPackage(t *testing.T) {
	pkg := linuxPackage("amd64")
	if pkg.Name != "awscli-exe-linux-x86_64.zip" || pkg.SigURL != "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip.sig" {
		t.Fatalf("%+v", pkg)
	}
	if linuxPackage("arm64").Name != "awscli-exe-linux-aarch64.zip" || linuxPackage("386").Name != "" {
		t.Fatal("architectures")
	}
	dir := t.TempDir()
	installed := false
	var verified [2]string
	var downloads []string
	pkg.Verify = func(file, sig string) (string, error) { verified = [2]string{file, sig}; return "AWS CLI Team", nil }
	pkg.Install = func(file, work, log string) (bool, error) { installed = true; return false, nil }
	in := Installer{
		Find: func() (string, error) {
			if installed {
				return "/usr/local/bin/aws", nil
			}
			return "", errors.New("absent")
		},
		Download: func(url, dst string) error {
			downloads = append(downloads, url)
			return os.WriteFile(dst, []byte("x"), 0o600)
		},
		Version: func(string) (string, error) { return "aws-cli/2.31.0 Python/3.13 Linux/6.8", nil },
		Package: pkg, TempDir: filepath.Join(dir, "work"), Out: &strings.Builder{},
	}
	provided := filepath.Join(dir, pkg.Name)
	os.WriteFile(provided, []byte("zip"), 0o600)
	if _, err := in.Ensure("", []string{provided}); err == nil || installed {
		t.Fatalf("paquet sans signature accepté : %v", err)
	}
	os.WriteFile(provided+".sig", []byte("sig"), 0o600)
	res, err := in.Ensure("", []string{provided})
	if err != nil || !installed || filepath.Dir(verified[0]) != in.TempDir || verified[1] != verified[0]+".sig" || res.Source != provided {
		t.Fatalf("%+v %v %v", res, err, verified)
	}
	installed = false
	if _, err := in.Ensure("", nil); err != nil || strings.Join(downloads, " ") != pkg.URL+" "+pkg.SigURL {
		t.Fatalf("%v %v", err, downloads)
	}
}
