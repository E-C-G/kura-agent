package setup

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// ServiceAccount : compte système sous lequel tourne le service kura-sync (Linux).
const ServiceAccount = "kura-agent"

// sysusersLine : compte système sans mot de passe ni shell, dossier /var/lib/kura,
// groupe du même nom (format sysusers.d).
const sysusersLine = `u kura-agent - "Kura, sauvegarde vers Wasabi" /var/lib/kura -`

// runSysusers crée le compte s'il manque ; remplaçable dans les tests.
var runSysusers = func(line string) error {
	out, err := exec.Command("systemd-sysusers", "--inline", line).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-sysusers : %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

var lookupUser = user.Lookup

// EnsureServiceAccount crée le compte kura-agent s'il n'existe pas encore (le paquet
// deb le crée aussi, par son fichier sysusers.d). created : vrai si créé maintenant.
func EnsureServiceAccount() (created bool, err error) {
	if _, err := lookupUser(ServiceAccount); err == nil {
		return false, nil
	}
	if err := runSysusers(sysusersLine); err != nil {
		return false, err
	}
	if _, err := lookupUser(ServiceAccount); err != nil {
		return false, fmt.Errorf("compte %s introuvable après création : %w", ServiceAccount, err)
	}
	return true, nil
}
