package creds

import (
	"os"

	"github.com/E-C-G/kura-agent/internal/platform"
)

// CredstoreDir : dossier standard des secrets chiffrés de systemd.
const CredstoreDir = "/etc/credstore.encrypted"

// System : coffre de production, chiffré par systemd-creds dans dir
// (CredstoreDir en production) ; secrets du service lus dans $CREDENTIALS_DIRECTORY.
// KURA_DEV=1 : en clair (développement uniquement).
func System(dir string) Store {
	if platform.DevMode() {
		return Store{Dir: dir}
	}
	return Store{Dir: dir, cipher: systemdCreds{run: runSystemdCreds}, prefix: Prefix,
		serviceDir: os.Getenv("CREDENTIALS_DIRECTORY")}
}
