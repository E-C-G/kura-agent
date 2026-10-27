package creds

import "github.com/E-C-G/kura-agent/internal/platform"

// System : coffre de production sous macOS, fichiers de dir (réservé à root) chiffrés
// par la clé du trousseau Système. KURA_DEV=1 : en clair (développement uniquement).
func System(dir string) Store {
	if platform.DevMode() {
		return Store{Dir: dir}
	}
	return Store{Dir: dir, cipher: newKeychainCipher(SystemKeychain, runSecurity)}
}
