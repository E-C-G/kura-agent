package awsinstall

import (
	"os"
	"path/filepath"
)

// SystemPackage : MSI pour toute la machine, signature Authenticode Amazon.
func SystemPackage() Package {
	return Package{
		Name: MSIName, URL: DownloadURL,
		Verify:  verifyMSI(signature),
		Install: func(file, _, logFile string) (bool, error) { return runMSI(file, logFile) },
	}
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
