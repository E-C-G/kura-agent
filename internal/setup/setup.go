// Package setup réunit les opérations système de `kura-agent install` et
// `uninstall` : compte de service, droits, ACL, copie des fichiers.
// Tout est propre à Windows ; ailleurs, chaque fonction renvoie une
// platform.NotImplementedError (fonction platform.Install).
package setup

import "github.com/E-C-G/kura-agent/internal/platform"

// ErrUnsupported : opération pas encore écrite pour ce système (errors.Is).
var ErrUnsupported = platform.ErrNotImplemented

func notImplemented() error { return platform.NotImplemented(platform.Install) }

// LegacyAccount est le compte de service des versions 0.3.0 à 0.4.1. La tâche tourne
// désormais sous SYSTEM ; install et uninstall suppriment ce compte s'il existe.
const LegacyAccount = "svc-kura"

// DataDirSDDL : C:\ProgramData\Kura. SYSTEM (la tâche) et Administrateurs en
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
