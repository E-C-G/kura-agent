package creds

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Prefix : préfixe des secrets de Kura dans /etc/credstore.encrypted (et dans
// $CREDENTIALS_DIRECTORY), importés par le service avec ImportCredential=kura.*.
const Prefix = "kura."

// systemdCreds chiffre avec systemd-creds : clé de la machine
// (/var/lib/systemd/credential.secret, root seul) et puce TPM si elle existe. Le nom
// est inscrit dans le chiffré : un fichier renommé est refusé.
type systemdCreds struct {
	// run lance systemd-creds avec stdin ; remplaçable dans les tests.
	run func(stdin []byte, args ...string) ([]byte, error)
}

func (c systemdCreds) protect(name string, plain []byte) ([]byte, error) {
	return c.run(plain, "encrypt", "--name="+name, "-", "-")
}

func (c systemdCreds) unprotect(name string, enc []byte) ([]byte, error) {
	return c.run(enc, "decrypt", "--name="+name, "-", "-")
}

func runSystemdCreds(stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("systemd-creds", args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("systemd-creds %s : %v %s (lancer avec sudo)", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
