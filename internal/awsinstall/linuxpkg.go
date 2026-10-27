package awsinstall

// linuxPackage : zip officiel d'une architecture Go (amd64, arm64) et sa signature
// PGP. Indépendant du système de compilation : se teste partout.
func linuxPackage(goarch string) Package {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
	if arch == "" {
		return Package{}
	}
	name := "awscli-exe-linux-" + arch + ".zip"
	url := "https://awscli.amazonaws.com/" + name
	return Package{
		Name: name, URL: url, SigURL: url + ".sig",
		Verify: func(file, sig string) (string, error) {
			return verifyPGP(file, sig, awsCLIKey, AWSCLIKeyFingerprint, runGPGV)
		},
		Install: installZip(runLogged),
	}
}
