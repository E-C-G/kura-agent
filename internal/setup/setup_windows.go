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
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/E-C-G/kura-agent/internal/procs"
)

var (
	netapi32       = windows.NewLazySystemDLL("netapi32.dll")
	procNetUserDel = netapi32.NewProc("NetUserDel")

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

// RemoveAccount supprime le compte de service d'une version précédente : droits LSA,
// masquage de l'écran d'accueil, profil, compte. Absent : rien à faire.
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

// GUIName est la version sans console de l'exécutable (icône, commandes élevées).
const GUIName = "kura-agentw.exe"

// InstallBinary copie l'exécutable courant dans dir, avec kura-agentw.exe s'il est à
// côté, et renvoie leurs chemins (gui vide s'il manque). Un binaire en cours
// d'exécution (icônes ouvertes) ne peut pas être écrasé mais peut être renommé :
// l'ancien devient .old, supprimé à l'installation suivante.
func InstallBinary(dir string) (exe, gui string, err error) {
	src, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if err := applySDDL(dir, ProgramDirSDDL); err != nil {
		return "", "", fmt.Errorf("droits de %s : %w", dir, err)
	}
	exe = filepath.Join(dir, "kura-agent.exe")
	if err := replaceFile(src, exe); err != nil {
		return "", "", err
	}
	srcGUI := filepath.Join(filepath.Dir(src), GUIName)
	if strings.EqualFold(filepath.Base(src), GUIName) {
		srcGUI = src
	}
	if _, err := os.Stat(srcGUI); err != nil {
		return exe, "", nil
	}
	gui = filepath.Join(dir, GUIName)
	if err := replaceFile(srcGUI, gui); err != nil {
		return exe, "", err
	}
	return exe, gui, nil
}

// replaceFile remplace dst par une copie de src. Une version en cours d'exécution (une
// icône ouverte dans une autre session) est renommée sous un nom unique, effacé au
// redémarrage ; les anciennes copies libérées sont supprimées au passage.
func replaceFile(src, dst string) error {
	if samePath(src, dst) {
		return nil
	}
	if olds, _ := filepath.Glob(dst + ".old*"); len(olds) > 0 {
		for _, o := range olds {
			_ = os.Remove(o)
		}
	}
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			old := fmt.Sprintf("%s.old-%d", dst, time.Now().UnixNano())
			if err := os.Rename(dst, old); err != nil {
				os.Remove(tmp)
				return fmt.Errorf("remplacement de %s : %w", dst, err)
			}
			_ = deleteAtReboot(old)
		}
	}
	return os.Rename(tmp, dst)
}

const runKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`

// SetTrayAutostart lance l'icône à chaque ouverture de session (tous les comptes ;
// elle se ferme aussitôt pour ceux qui ne sont pas dans tray.users).
func SetTrayAutostart(gui string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("Kura", `"`+gui+`" tray`)
}

// RemoveTrayAutostart retire le lancement de l'icône ; absent n'est pas une erreur.
func RemoveTrayAutostart() error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, runKey, registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	if err := k.DeleteValue("Kura"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// StopTrays ferme les icônes ouvertes dans toutes les sessions et renvoie leur nombre.
func StopTrays() int {
	m := procs.System()
	found, err := m.Find(GUIName)
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range found {
		if m.Terminate(p) == nil {
			n++
		}
	}
	return n
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
			// Exécutable en cours (uninstall lui-même, une icône restée ouverte) :
			// effacé au redémarrage.
			lower := strings.ToLower(p)
			if !samePath(p, self) && !strings.Contains(lower, ".old") && !strings.HasSuffix(lower, ".exe") {
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
