package awsinstall

import (
	"errors"
	"strings"
	"testing"
)

const goodPkgutil = `Package "AWSCLIV2.pkg":
   Status: signed by a developer certificate issued by Apple for distribution
   Notarization: trusted by the Apple notary service
   Signed with a trusted timestamp on: 2026-10-20 18:03:44 +0000
   Certificate Chain:
    1. Developer ID Installer: AMZN Mobile LLC (94KV3E626L)
       Expires: 2030-01-01 00:00:00 +0000
       SHA256 Fingerprint:
           00 11 22
       ------------------------------------------------------------------------
    2. Developer ID Certification Authority
       Expires: 2031-09-17 00:00:00 +0000
    3. Apple Root CA
       Expires: 2035-02-09 21:40:36 +0000
`

func TestCheckPkgSignature(t *testing.T) {
	if s, err := CheckPkgSignature(goodPkgutil, AmazonTeamID); err != nil || s != "AMZN Mobile LLC (94KV3E626L)" {
		t.Fatalf("%q %v", s, err)
	}
	bad := map[string]string{
		"non signé":      strings.Replace(goodPkgutil, "signed by a developer certificate issued by Apple for distribution", "no signature", 1),
		"autre équipe":   strings.Replace(goodPkgutil, "(94KV3E626L)", "(ABCDE12345)", 1),
		"non notarisé":   strings.Replace(goodPkgutil, "   Notarization: trusted by the Apple notary service\n", "", 1),
		"certificat dev": strings.Replace(goodPkgutil, "signed by a developer certificate issued by Apple for distribution", "signed by a developer certificate issued by Apple (Development)", 1),
		"sans chaîne":    strings.Split(goodPkgutil, "   Certificate Chain:")[0],
		"nom trompeur":   strings.Replace(goodPkgutil, "1. Developer ID Installer: AMZN Mobile LLC (94KV3E626L)", "1. Developer ID Installer: AMZN Mobile LLC (94KV3E626L) Mallory (ABCDE12345)", 1),
		"équipe en 2e":   strings.Replace(strings.Replace(goodPkgutil, "(94KV3E626L)", "(ABCDE12345)", 1), "2. Developer ID Certification Authority", "2. Developer ID Installer: AMZN Mobile LLC (94KV3E626L)", 1),
		"sortie vide":    "",
	}
	for name, out := range bad {
		if s, err := CheckPkgSignature(out, AmazonTeamID); err == nil {
			t.Errorf("%s : accepté (%s)", name, s)
		}
	}
}

func TestMacPackage(t *testing.T) {
	var ran []string
	p := macPackage(func(string) (string, error) { return goodPkgutil, nil },
		func(log, name string, args ...string) error { ran = append([]string{name}, args...); return nil })
	if signer, err := p.Verify("/tmp/x/AWSCLIV2.pkg", ""); err != nil || !strings.Contains(signer, AmazonTeamID) {
		t.Fatal(signer, err)
	}
	if reboot, err := p.Install("/tmp/x/AWSCLIV2.pkg", "/tmp/x", "/tmp/x/log"); err != nil || reboot {
		t.Fatal(err)
	}
	if strings.Join(ran, " ") != "/usr/sbin/installer -pkg /tmp/x/AWSCLIV2.pkg -target /" {
		t.Fatal(ran)
	}
	if p.SigName() != "" || p.Name != PkgName || p.URL != PkgURL {
		t.Fatalf("%+v", p)
	}
	// pkgutil en échec (paquet non signé : code 1, sortie lisible) : refus motivé.
	p = macPackage(func(string) (string, error) {
		return "Package \"AWSCLIV2.pkg\":\n   Status: no signature\n", errors.New("exit status 1")
	}, nil)
	if _, err := p.Verify("x", ""); err == nil || !strings.Contains(err.Error(), "no signature") {
		t.Fatal(err)
	}
}
