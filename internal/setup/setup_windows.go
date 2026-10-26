//go:build windows

package setup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	netapi32                    = windows.NewLazySystemDLL("netapi32.dll")
	procNetUserAdd              = netapi32.NewProc("NetUserAdd")
	procNetUserDel              = netapi32.NewProc("NetUserDel")
	procNetLocalGroupAddMembers = netapi32.NewProc("NetLocalGroupAddMembers")

	advapi32                   = windows.NewLazySystemDLL("advapi32.dll")
	procLsaOpenPolicy          = advapi32.NewProc("LsaOpenPolicy")
	procLsaAddAccountRights    = advapi32.NewProc("LsaAddAccountRights")
	procLsaRemoveAccountRights = advapi32.NewProc("LsaRemoveAccountRights")
	procLsaClose               = advapi32.NewProc("LsaClose")
	procLsaNtStatusToWinError  = advapi32.NewProc("LsaNtStatusToWinError")

	userenv           = windows.NewLazySystemDLL("userenv.dll")
	procDeleteProfile = userenv.NewProc("DeleteProfileW")
)

const (
	nerrUserExists      = 2224
	nerrUserNotFound    = 2221
	errorMemberInAlias  = 1378
	userPrivUser        = 1
	ufScript            = 0x0001
	ufPasswdCantChange  = 0x0040
	ufNormalAccount     = 0x0200
	ufDontExpirePasswd  = 0x10000
	policyAllAccess     = 0x000F0FFF
	verNTDomainCtrl     = 2
	specialAccountsPath = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList`
)

// userInfo1 est USER_INFO_1.
type userInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Priv        uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

// CheckAdmin vérifie que le processus tourne élevé.
func CheckAdmin() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("droits d'administrateur requis : relancer depuis une invite de commandes « Exécuter en tant qu'administrateur »")
	}
	if windows.RtlGetVersion().ProductType == verNTDomainCtrl {
		return errors.New("contrôleur de domaine : pas de compte local possible, installation non prise en charge")
	}
	return nil
}

// qualified renvoie MACHINE\nom : on ne veut jamais tomber sur un compte du domaine homonyme.
func qualified(name string) (string, error) {
	host, err := windows.ComputerName()
	if err != nil {
		return "", err
	}
	return host + `\` + name, nil
}

// LocalAccountSID renvoie le SID du compte local, ou une erreur s'il n'existe pas.
func LocalAccountSID(name string) (string, error) {
	q, err := qualified(name)
	if err != nil {
		return "", err
	}
	sid, _, typ, err := windows.LookupSID("", q)
	if err != nil {
		return "", err
	}
	if typ != windows.SidTypeUser {
		return "", fmt.Errorf("%s n'est pas un compte utilisateur", q)
	}
	return sid.String(), nil
}

// AccountSID renvoie le SID d'un compte (local en priorité, sinon du domaine), pour
// les comptes de tray.allow_run_now.
func AccountSID(name string) (string, error) {
	if !strings.Contains(name, `\`) {
		if sid, err := LocalAccountSID(name); err == nil {
			return sid, nil
		}
	}
	sid, _, typ, err := windows.LookupSID("", name)
	if err != nil {
		return "", err
	}
	if typ != windows.SidTypeUser {
		return "", fmt.Errorf("%s n'est pas un compte utilisateur", name)
	}
	return sid.String(), nil
}

// EnsureAccount crée le compte de service s'il manque, l'ajoute aux Administrateurs,
// lui donne ses droits et le masque de l'écran d'accueil. Rejouable sans effet de bord :
// un compte existant garde son mot de passe.
func EnsureAccount(name string) (sid string, created bool, err error) {
	if _, err := LocalAccountSID(name); err != nil {
		pw, err := Password(40)
		if err != nil {
			return "", false, err
		}
		if err := netUserAdd(name, pw); err != nil {
			return "", false, fmt.Errorf("création du compte %s : %w", name, err)
		}
		created = true
	}
	sid, err = LocalAccountSID(name)
	if err != nil {
		return "", created, err
	}
	s, err := windows.StringToSid(sid)
	if err != nil {
		return "", created, err
	}
	if err := addToAdministrators(s); err != nil {
		return sid, created, fmt.Errorf("ajout aux Administrateurs : %w", err)
	}
	if err := lsaRights(s, AccountRights, false); err != nil {
		return sid, created, fmt.Errorf("droits du compte : %w", err)
	}
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, specialAccountsPath, registry.SET_VALUE)
	if err != nil {
		return sid, created, fmt.Errorf("masquage de l'écran d'accueil : %w", err)
	}
	defer k.Close()
	if err := k.SetDWordValue(name, 0); err != nil {
		return sid, created, fmt.Errorf("masquage de l'écran d'accueil : %w", err)
	}
	return sid, created, nil
}

// RemoveAccount défait EnsureAccount et supprime le profil du compte. Absent : rien à faire.
func RemoveAccount(name string) error {
	sid, err := LocalAccountSID(name)
	if err != nil {
		return nil
	}
	var errs []error
	if s, err := windows.StringToSid(sid); err == nil {
		if err := lsaRights(s, nil, true); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			errs = append(errs, fmt.Errorf("retrait des droits : %w", err))
		}
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, specialAccountsPath, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue(name)
		k.Close()
	}
	// Le profil d'abord : DeleteProfile a besoin du SID encore résolu.
	if p, err := windows.UTF16PtrFromString(sid); err == nil {
		if r, _, e := procDeleteProfile.Call(uintptr(unsafe.Pointer(p)), 0, 0); r == 0 &&
			!errors.Is(e, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(e, windows.ERROR_PATH_NOT_FOUND) {
			errs = append(errs, fmt.Errorf("suppression du profil : %w", e))
		}
	}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	if r, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(n))); r != 0 && r != nerrUserNotFound {
		errs = append(errs, fmt.Errorf("suppression du compte : %w", syscall.Errno(r)))
	}
	return errors.Join(errs...)
}

func netUserAdd(name, password string) error {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	c, _ := windows.UTF16PtrFromString("Kura : exécute la sauvegarde planifiée. Ne pas utiliser pour ouvrir une session.")
	info := userInfo1{Name: n, Password: p, Priv: userPrivUser, Comment: c,
		Flags: ufScript | ufNormalAccount | ufDontExpirePasswd | ufPasswdCantChange}
	var parmErr uint32
	r, _, _ := procNetUserAdd.Call(0, 1, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&parmErr)))
	switch r {
	case 0, nerrUserExists:
		return nil
	default:
		return fmt.Errorf("NetUserAdd : erreur %d (paramètre %d)", r, parmErr)
	}
}

func addToAdministrators(sid *windows.SID) error {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	// Nom localisé du groupe (« Administrateurs » sur un Windows français).
	group, _, _, err := admins.LookupAccount("")
	if err != nil {
		return err
	}
	g, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	member := struct{ sid *windows.SID }{sid} // LOCALGROUP_MEMBERS_INFO_0
	r, _, _ := procNetLocalGroupAddMembers.Call(0, uintptr(unsafe.Pointer(g)), 0, uintptr(unsafe.Pointer(&member)), 1)
	if r != 0 && r != errorMemberInAlias {
		return syscall.Errno(r)
	}
	return nil
}

// lsaRights ajoute des droits, ou retire tous ceux du compte (removeAll).
func lsaRights(sid *windows.SID, rights []string, removeAll bool) error {
	var attrs windows.OBJECT_ATTRIBUTES
	var policy windows.Handle
	if st, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&attrs)), policyAllAccess,
		uintptr(unsafe.Pointer(&policy))); st != 0 {
		return ntErr(st)
	}
	defer procLsaClose.Call(uintptr(policy))
	if removeAll {
		if st, _, _ := procLsaRemoveAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)), 1, 0, 0); st != 0 {
			return ntErr(st)
		}
		return nil
	}
	list := make([]windows.NTUnicodeString, len(rights))
	for i, r := range rights {
		u, err := windows.NewNTUnicodeString(r)
		if err != nil {
			return err
		}
		list[i] = *u
	}
	if st, _, _ := procLsaAddAccountRights.Call(uintptr(policy), uintptr(unsafe.Pointer(sid)),
		uintptr(unsafe.Pointer(&list[0])), uintptr(len(list))); st != 0 {
		return ntErr(st)
	}
	return nil
}

func ntErr(status uintptr) error {
	r, _, _ := procLsaNtStatusToWinError.Call(status)
	return syscall.Errno(r)
}

// SecureDataDir crée le dossier de données et lui applique DataDirSDDL.
func SecureDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return applySDDL(dir, DataDirSDDL)
}

func applySDDL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// ProgramDir est C:\Program Files\Kura.
func ProgramDir() string {
	base := os.Getenv("ProgramFiles")
	if base == "" {
		base = `C:\Program Files`
	}
	return filepath.Join(base, "Kura")
}

// InstallBinary copie l'exécutable courant dans dir et renvoie son chemin. Un binaire
// en cours d'exécution ne peut pas être écrasé mais peut être renommé : l'ancien
// devient kura-agent.exe.old, supprimé à l'installation suivante.
func InstallBinary(dir string) (string, error) {
	src, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "kura-agent.exe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := applySDDL(dir, ProgramDirSDDL); err != nil {
		return "", fmt.Errorf("droits de %s : %w", dir, err)
	}
	if samePath(src, dst) {
		return dst, nil
	}
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		return "", err
	}
	old := dst + ".old"
	_ = os.Remove(old)
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			os.Remove(tmp)
			return "", fmt.Errorf("remplacement de %s : %w", dst, err)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// RemoveProgramDir supprime C:\Program Files\Kura. L'exécutable en cours (uninstall
// lancé depuis ce dossier) et le dossier sont effacés au prochain redémarrage.
func RemoveProgramDir(dir string) (pendingReboot bool, err error) {
	self, _ := os.Executable()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			if !samePath(p, self) && !strings.HasSuffix(strings.ToLower(p), ".old") {
				return pendingReboot, err
			}
			if err := deleteAtReboot(p); err != nil {
				return pendingReboot, err
			}
			pendingReboot = true
		}
	}
	if pendingReboot {
		return true, deleteAtReboot(dir)
	}
	return false, os.Remove(dir)
}

func deleteAtReboot(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

func samePath(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	if err1 == nil {
		a = ea
	}
	if err2 == nil {
		b = eb
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
