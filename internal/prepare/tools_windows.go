//go:build windows

package prepare

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"unsafe"

	"github.com/E-C-G/kura-agent/internal/secdir"
	"golang.org/x/sys/windows"
)

// Droits qui permettraient à un compte de remplacer l'outil ou de faire charger son
// code à côté de lui.
const (
	fileWriteData   = 0x2  // fichier : écrire ; dossier : ajouter un fichier
	fileAppendData  = 0x4  // fichier : ajouter ; dossier : ajouter un sous-dossier
	fileWriteEA     = 0x10 // attributs étendus
	fileDeleteChild = 0x40 // dossier : supprimer ce qu'il contient
	genericAll      = 0x10000000
	genericWrite    = 0x40000000
	takeOver        = windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | genericAll | genericWrite

	exeForbidden    = takeOver | fileWriteData | fileAppendData | fileWriteEA
	parentForbidden = takeOver | fileDeleteChild | fileWriteData | fileAppendData // DLL posée à côté
	upperForbidden  = takeOver | fileDeleteChild                                  // dossier renommé ou remplacé
	// Racine du volume : elle ne peut être ni supprimée ni renommée, DELETE sur elle ne
	// donne rien (certains postes l'accordent aux utilisateurs authentifiés, avec
	// « Modification ») ; seuls comptent ses droits et ce qu'elle contient.
	rootForbidden = takeOver&^windows.DELETE | fileDeleteChild
)

// adminOnly vérifie l'outil, ses dossiers parents, puis tout ce que contient son dossier
// (7-Zip y charge 7z.dll et les DLL de Codecs et Formats).
func adminOnly(path string) error {
	if err := chainAdminOnly(path); err != nil {
		return err
	}
	return treeAdminOnly(filepath.Dir(filepath.Clean(path)))
}

// maxToolTree : au-delà, le dossier de l'outil est trop grand pour être vérifié en entier
// (outil posé dans un dossier partagé comme System32) : refusé.
const maxToolTree = 5000

// treeAdminOnly : chaque fichier et sous-dossier du dossier de l'outil est réservé aux
// administrateurs, sans point de reparse.
func treeAdminOnly(dir string) error {
	n := 0
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		if n++; n > maxToolTree {
			return fmt.Errorf("%s contient trop de fichiers pour être vérifié : installer l'outil dans son propre dossier", dir)
		}
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(p))
			if err != nil || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return fmt.Errorf("%s est une jonction ou un lien, dans le dossier de l'outil : refusé", p)
			}
		}
		forbidden := uint32(exeForbidden)
		if d.IsDir() {
			forbidden = parentForbidden
		}
		return checkACL(p, forbidden)
	})
}

// chainAdminOnly : ni l'outil, ni son dossier, ni les dossiers au-dessus ne sont des points de
// reparse, n'appartiennent à un compte non administrateur, ni n'accordent à un tel compte
// le droit de les modifier (entrées « héritage seul » ignorées). À la racine du volume,
// où tout utilisateur peut créer un dossier, seuls les droits de remplacement sont
// refusés ; un outil posé directement à la racine est refusé.
func chainAdminOnly(path string) error {
	p := filepath.Clean(path)
	for level := 0; ; level++ {
		parent := filepath.Dir(p)
		root := parent == p
		if !root {
			attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(p))
			if err != nil {
				return fmt.Errorf("%s : %w", p, err)
			}
			if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return fmt.Errorf("%s est une jonction ou un lien : l'outil doit être dans un vrai dossier, réservé aux administrateurs", p)
			}
		}
		forbidden := uint32(upperForbidden)
		switch level {
		case 0:
			forbidden = exeForbidden
		case 1:
			if root { // tout utilisateur peut créer un dossier à la racine (Codecs…)
				return fmt.Errorf("%s : outil posé à la racine du volume ; l'installer dans son propre dossier (ex. C:\\Program Files)", path)
			}
			forbidden = parentForbidden
		default:
			if root {
				forbidden = rootForbidden
			}
		}
		if err := checkACL(p, forbidden); err != nil {
			return err
		}
		if root {
			return nil
		}
		p = parent
	}
}

func checkACL(p string, forbidden uint32) error {
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("droits de %s : %w", p, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("propriétaire de %s : %w", p, err)
	}
	if !secdir.TrustedOwner(owner) {
		return fmt.Errorf("%s appartient à %s, qui n'est pas administrateur : il pourrait remplacer l'outil", p, account(owner))
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("droits de %s : %w", p, err)
	}
	if dacl == nil {
		return fmt.Errorf("%s n'a aucune liste de droits : tout le monde peut le modifier", p)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("droits de %s : %w", p, err)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		case accessDeniedACE, accessDeniedObjectACE, accessDeniedCallbackACE, accessDeniedCallbackObjectACE:
			continue // un refus ne donne aucun droit
		default: // autorisation conditionnelle, objet… : non reconnue, donc refusée
			return fmt.Errorf("%s : entrée de droits de type %d non reconnue ; l'outil doit être dans un dossier aux droits standard", p, ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if trustedSID(sid) {
			continue
		}
		if uint32(ace.Mask)&forbidden != 0 {
			return fmt.Errorf("%s : %s peut le modifier (droits 0x%x) ; l'outil doit être dans un dossier réservé aux administrateurs (ex. C:\\Program Files)", p, account(sid), uint32(ace.Mask))
		}
	}
	return nil
}

// Types d'entrée de refus (winnt.h).
const (
	accessDeniedACE               = 1
	accessDeniedObjectACE         = 6
	accessDeniedCallbackACE       = 10
	accessDeniedCallbackObjectACE = 12
)

var trustedInstaller, _ = windows.StringToSid("S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464")

// trustedSID : SYSTEM, Administrateurs, TrustedInstaller, CREATOR OWNER (remplacé par le
// propriétaire, déjà vérifié).
func trustedSID(sid *windows.SID) bool {
	return sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
		sid.IsWellKnown(windows.WinCreatorOwnerSid) || trustedInstaller != nil && sid.Equals(trustedInstaller)
}

func account(sid *windows.SID) string {
	if a, d, _, err := sid.LookupAccount(""); err == nil {
		return d + `\` + a
	}
	return sid.String()
}
