// Package setup réunit les opérations système de `kura-agent install` et
// `uninstall` : compte de service, droits, ACL, copie des fichiers.
// Tout est propre à Windows ; ailleurs, chaque fonction renvoie ErrUnsupported.
package setup

import (
	"crypto/rand"
	"errors"
	"math/big"
)

// ErrUnsupported : opération réservée à Windows.
var ErrUnsupported = errors.New("installation : Windows uniquement")

// DefaultAccount est le compte local qui exécute la tâche planifiée.
const DefaultAccount = "svc-kura"

// AccountRights : droits accordés au compte de service. Il ouvre une session de type
// tâche (S4U) et ne peut ouvrir ni session locale ni Bureau à distance. Son mot de
// passe aléatoire n'est connu de personne : aucune ouverture de session réseau possible.
var AccountRights = []string{
	"SeBatchLogonRight",
	"SeDenyInteractiveLogonRight",
	"SeDenyRemoteInteractiveLogonRight",
}

// DataDirSDDL : C:\ProgramData\Kura. SYSTEM et Administrateurs (dont svc-kura) en
// contrôle total, Utilisateurs en lecture (config, état, journaux pour l'icône).
// ACL protégée : les Utilisateurs ne peuvent plus y créer de fichier, contrairement
// au reste de C:\ProgramData. Le sous-dossier credentials a sa propre ACL, sans les
// Utilisateurs.
const DataDirSDDL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"

// ProgramDirSDDL : C:\Program Files\Kura. L'exécutable lancé par la tâche (compte
// administrateur, démarrable par les comptes de tray.allow_run_now) ne doit être
// modifiable que par SYSTEM et les Administrateurs ; les Utilisateurs le lisent et
// l'exécutent.
const ProgramDirSDDL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"

// Password renvoie un mot de passe aléatoire de n caractères, avec au moins une
// majuscule, une minuscule, un chiffre et un symbole (stratégie de complexité Windows).
// Avec une tâche S4U, personne n'a besoin de le connaître : il n'est ni affiché ni gardé.
func Password(n int) (string, error) {
	const (
		upper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		lower  = "abcdefghijkmnopqrstuvwxyz"
		digits = "23456789"
		symbol = "!#%+-.=?@_~"
	)
	classes := []string{upper, lower, digits, symbol}
	all := upper + lower + digits + symbol
	if n < len(classes) {
		return "", errors.New("mot de passe trop court")
	}
	pick := func(set string) (byte, error) {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return 0, err
		}
		return set[i.Int64()], nil
	}
	b := make([]byte, n)
	for i := range b {
		set := all
		if i < len(classes) {
			set = classes[i]
		}
		c, err := pick(set)
		if err != nil {
			return "", err
		}
		b[i] = c
	}
	// Mélange (Fisher-Yates) pour ne pas laisser les classes imposées en tête.
	for i := len(b) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		b[i], b[j.Int64()] = b[j.Int64()], b[i]
	}
	return string(b), nil
}
