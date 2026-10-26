package awsinstall

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func systemExe(parts ...string) string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// signature lit la signature Authenticode du MSI avec Get-AuthenticodeSignature
// (WinVerifyTrust : chaîne de certificats, révocation, horodatage). Le chemin passe
// par l'environnement, jamais dans le texte de la commande.
func signature(msi string) (status, subject string, err error) {
	ps := systemExe("System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	script := `$ErrorActionPreference='Stop'; [Console]::OutputEncoding=[Text.Encoding]::UTF8; ` +
		`$s=Get-AuthenticodeSignature -LiteralPath $env:KURA_MSI; ` +
		`Write-Output $s.Status.ToString(); if ($s.SignerCertificate) { Write-Output $s.SignerCertificate.Subject }`
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "KURA_MSI="+msi)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", "", fmt.Errorf("powershell : %v %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", "", fmt.Errorf("powershell : %w", err)
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(string(out)), "\r\n", "\n"), "\n")
	status = strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		subject = strings.TrimSpace(lines[1])
	}
	return status, subject, nil
}

// runMSI : installation silencieuse pour toute la machine, sans redémarrage.
func runMSI(msi, logFile string) (bool, error) {
	cmd := exec.Command(systemExe("System32", "msiexec.exe"), "/i", msi, "/qn", "/norestart", "/l*v", logFile)
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 3010, 1641:
			return true, nil
		case 1618:
			return false, errors.New("une autre installation Windows est en cours, réessayer dans quelques minutes")
		case 1603:
			return false, errors.New("msiexec : échec de l'installation (1603)")
		}
		return false, fmt.Errorf("msiexec : code %d", ee.ExitCode())
	}
	return false, err
}
