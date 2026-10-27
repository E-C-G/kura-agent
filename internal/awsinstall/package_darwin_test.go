package awsinstall

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Avec le vrai paquet d'AWS et le vrai pkgutil (CI macOS) : signature, équipe et
// architectures de la CLI.
func TestRealAWSCLIPkg(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("CI macOS seulement")
	}
	dir := t.TempDir()
	pkg := filepath.Join(dir, PkgName)
	if err := HTTPDownload(PkgURL, pkg); err != nil {
		t.Fatal(err)
	}
	out, _ := pkgutilCheck(pkg)
	signer, err := CheckPkgSignature(out, AmazonTeamID)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("AWSCLIV2.pkg signé par %s", signer)
	exp := filepath.Join(dir, "x")
	if b, err := exec.Command("/usr/sbin/pkgutil", "--expand-full", pkg, exp).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	var bins []string
	filepath.Walk(exp, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && fi.Name() == "aws" {
			bins = append(bins, p)
		}
		return nil
	})
	if len(bins) == 0 {
		t.Fatal("aws absent du paquet")
	}
	archs, err := exec.Command("lipo", "-archs", bins[0]).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, archs)
	}
	t.Logf("architectures de %s : %s", strings.TrimPrefix(bins[0], exp), strings.TrimSpace(string(archs)))
	if a := string(archs); !strings.Contains(a, "x86_64") || !strings.Contains(a, "arm64") {
		t.Errorf("paquet non universel : %s", a)
	}
}
