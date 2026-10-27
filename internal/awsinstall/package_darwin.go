package awsinstall

import "os/exec"

// SystemPackage : AWSCLIV2.pkg, signature Amazon vérifiée par pkgutil.
func SystemPackage() Package { return macPackage(pkgutilCheck, runLogged) }

// DefaultPath : lien créé par l'installeur.
func DefaultPath() string { return macBinDir + "/aws" }

func pkgutilCheck(file string) (string, error) {
	out, err := exec.Command("/usr/sbin/pkgutil", "--check-signature", file).CombinedOutput()
	return string(out), err
}
