package awsinstall

import (
	"fmt"
	"regexp"
	"strings"
)

// macOS : AWSCLIV2.pkg, installeur signé par Amazon (certificat Developer ID
// Installer de l'équipe AmazonTeamID) et notarisé par Apple, vérifié par
// « pkgutil --check-signature » puis installé par « installer -pkg … -target / »
// (/usr/local/aws-cli, lien /usr/local/bin/aws).
const (
	// PkgName : nom attendu du paquet posé à côté du paquet .kura.
	PkgName = "AWSCLIV2.pkg"
	// PkgURL : dernière version de la CLI AWS v2 pour macOS.
	PkgURL = "https://awscli.amazonaws.com/AWSCLIV2.pkg"
	// AmazonTeamID : équipe Apple qui signe AWSCLIV2.pkg (« AMZN Mobile LLC ») ; le nom
	// affiché a déjà changé, l'identifiant d'équipe non.
	AmazonTeamID = "94KV3E626L"
	// macBinDir : lien créé par l'installeur.
	macBinDir = "/usr/local/bin"
)

// MacSearchDirs : dossiers où la CLI est cherchée sous macOS, ceux du PATH donné au
// démon launchd (internal/launchd) : une CLI ailleurs (Homebrew dans /opt/homebrew)
// marcherait avec sudo mais pas dans la sauvegarde planifiée.
var MacSearchDirs = []string{"/usr/local/bin", "/usr/bin", "/bin"}

var chainSigner = regexp.MustCompile(`^\s*1\.\s+Developer ID Installer:\s+(.+)\s+\(([A-Z0-9]{10})\)\s*$`)

// CheckPkgSignature lit la sortie de « pkgutil --check-signature » : paquet signé par
// un certificat Developer ID de l'équipe team et notarisé par Apple. Renvoie le
// signataire (« AMZN Mobile LLC (94KV3E626L) »).
func CheckPkgSignature(out, team string) (string, error) {
	var status, notarized, signer, signerTeam string
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "Status:"):
			status = strings.TrimSpace(strings.TrimPrefix(t, "Status:"))
		case strings.HasPrefix(t, "Notarization:"):
			notarized = strings.TrimSpace(strings.TrimPrefix(t, "Notarization:"))
		}
		if m := chainSigner.FindStringSubmatch(l); m != nil && signer == "" {
			signer, signerTeam = m[1], m[2]
		}
	}
	switch {
	case status != "signed by a developer certificate issued by Apple for distribution":
		if status == "" {
			status = "absente"
		}
		return "", fmt.Errorf("signature du paquet : %s", status)
	case notarized != "trusted by the Apple notary service":
		return "", fmt.Errorf("paquet non notarisé par Apple (%q)", notarized)
	case signer == "":
		return "", fmt.Errorf("signataire Developer ID Installer introuvable")
	case signerTeam != team:
		return "", fmt.Errorf("signé par %s (%s), pas par l'équipe Amazon %s", signer, signerTeam, team)
	}
	return fmt.Sprintf("%s (%s)", signer, signerTeam), nil
}

// macPackage : AWSCLIV2.pkg, vérifié avec check (sortie de pkgutil) et installé avec
// run (commande journalisée).
func macPackage(check func(file string) (string, error), run func(logFile, name string, args ...string) error) Package {
	return Package{
		Name: PkgName, URL: PkgURL,
		Verify: func(file, _ string) (string, error) {
			out, err := check(file)
			if err != nil && out == "" {
				return "", fmt.Errorf("vérification de la signature : %w", err)
			}
			return CheckPkgSignature(out, AmazonTeamID)
		},
		Install: func(file, _, logFile string) (bool, error) {
			return false, run(logFile, "/usr/sbin/installer", "-pkg", file, "-target", "/")
		},
	}
}
