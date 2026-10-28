//go:build windows

package secdir

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	netapi32                  = windows.NewLazySystemDLL("netapi32.dll")
	procNetUserGetLocalGroups = netapi32.NewProc("NetUserGetLocalGroups")
)

// Secure crée dir s'il manque (avec ses parents), refuse un point de reparse ou un
// propriétaire étranger, puis pose sddl (DACL protégée) et le propriétaire
// Administrateurs. Les contrôles portent sur le dossier tel qu'il existe après la
// création : un dossier créé entre-temps par un autre compte est refusé.
func Secure(dir, sddl string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := checkNotReparse(dir); err != nil {
		return err
	}
	if err := checkOwner(dir); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	// La DACL d'abord (elle donne le contrôle total aux Administrateurs, donc le droit
	// de changer le propriétaire), puis le propriétaire.
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION, admins, nil, nil, nil); err != nil {
		return fmt.Errorf("propriétaire de %s : %w", dir, err)
	}
	return nil
}

func checkNotReparse(dir string) error {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return &ReparsePointError{Dir: dir}
	}
	if attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%s n'est pas un dossier", dir)
	}
	return nil
}

func checkOwner(dir string) error {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("propriétaire de %s : %w", dir, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("propriétaire de %s : %w", dir, err)
	}
	if trustedOwner(owner) {
		return nil
	}
	name := owner.String()
	if acct, dom, _, err := owner.LookupAccount(""); err == nil {
		name = dom + `\` + acct
	}
	return &ForeignOwnerError{Dir: dir, Owner: name}
}

// trustedOwner : SYSTEM, TrustedInstaller, le groupe Administrateurs, le compte qui
// lance l'installation (élevé), ou un membre du groupe Administrateurs.
func trustedOwner(owner *windows.SID) bool {
	if owner.IsWellKnown(windows.WinLocalSystemSid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return true
	}
	if ti, err := windows.StringToSid("S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"); err == nil && owner.Equals(ti) {
		return true
	}
	if u, err := windows.GetCurrentProcessToken().GetTokenUser(); err == nil && owner.Equals(u.User.Sid) {
		return true
	}
	return isAdministrator(owner)
}

// isAdministrator : le compte est membre (directement ou par un groupe) du groupe
// local Administrateurs. Un SID qui n'est pas un compte utilisateur répond non.
func isAdministrator(sid *windows.SID) bool {
	acct, dom, typ, err := sid.LookupAccount("")
	if err != nil || typ != windows.SidTypeUser {
		return false
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	adminsName, _, _, err := admins.LookupAccount("")
	if err != nil {
		return false
	}
	user, err := windows.UTF16PtrFromString(dom + `\` + acct)
	if err != nil {
		return false
	}
	const lgIncludeIndirect = 1
	var buf *byte
	var read, total uint32
	r, _, _ := procNetUserGetLocalGroups.Call(0, uintptr(unsafe.Pointer(user)), 0, lgIncludeIndirect,
		uintptr(unsafe.Pointer(&buf)), 0xFFFFFFFF, uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)))
	if buf != nil {
		defer windows.NetApiBufferFree(buf)
	}
	if r != 0 || buf == nil {
		return false
	}
	names := unsafe.Slice((**uint16)(unsafe.Pointer(buf)), read)
	for _, n := range names {
		if windows.UTF16PtrToString(n) == adminsName {
			return true
		}
	}
	return false
}

// IsReparsePoint : pour les tests et les appelants qui veulent un diagnostic.
func IsReparsePoint(err error) bool {
	var r *ReparsePointError
	return errors.As(err, &r)
}

// TrustedOwner : SYSTEM, TrustedInstaller, le groupe Administrateurs, le compte qui
// lance l'agent, ou un membre du groupe Administrateurs (voir trustedOwner).
func TrustedOwner(sid *windows.SID) bool { return trustedOwner(sid) }
