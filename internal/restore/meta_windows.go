package restore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// privateFileSDDL : SYSTEM et Administrateurs seuls, sans héritage.
	privateFileSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	// privateDirSDDL : de même, transmis aux fichiers et dossiers créés dedans.
	privateDirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
)

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procReOpenFile   = kernel32.NewProc("ReOpenFile")
	errNotAdminOwned = errors.New("dossier existant qui appartient à un autre compte : choisir un dossier absent, il sera créé réservé aux administrateurs")
)

// rights : descripteur de sécurité (DACL) d'un fichier.
type rights struct{ sd *windows.SECURITY_DESCRIPTOR }

// readRights lit l'ACL d'un fichier ouvert (le handle porte READ_CONTROL).
func readRights(f *os.File) (rights, error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return rights{}, err
	}
	return rights{sd: sd}, nil
}

func sddl(s string) rights {
	sd, err := windows.SecurityDescriptorFromString(s)
	if err != nil {
		panic(err) // constante
	}
	return rights{sd: sd}
}

// privateRights : administrateurs et SYSTEM seuls.
func privateRights() rights { return sddl(privateFileSDDL) }

// reopen : nouveau handle sur le même fichier, avec d'autres droits, sans passer par
// son chemin.
func reopen(f *os.File, access uint32) (windows.Handle, error) {
	h, _, err := procReOpenFile.Call(f.Fd(), uintptr(access),
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if windows.Handle(h) == windows.InvalidHandle {
		return 0, fmt.Errorf("ReOpenFile : %w", err)
	}
	return windows.Handle(h), nil
}

// applySD pose l'ACL de r sur le handle : entrées explicites et protection contre
// l'héritage (si r n'est pas protégée, l'héritage du dossier parent est recalculé).
func applySD(h windows.Handle, r rights) error {
	dacl, _, err := r.sd.DACL()
	if err != nil {
		return err
	}
	ctrl, _, err := r.sd.Control()
	if err != nil {
		return err
	}
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if ctrl&windows.SE_DACL_PROTECTED != 0 {
		flags |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		flags |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil)
}

// applyRights pose l'ACL sur le fichier ouvert, par un second handle (WRITE_DAC) obtenu
// depuis le premier : jamais par le chemin.
func applyRights(f *os.File, r rights) error {
	h, err := reopen(f, windows.WRITE_DAC|windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return applySD(h, r)
}

// setTimes date le fichier ouvert.
func setTimes(f *os.File, t time.Time) error {
	ft := windows.NsecToFiletime(t.UnixNano())
	return windows.SetFileTime(windows.Handle(f.Fd()), nil, nil, &ft)
}

// stagingDir : dossier de travail privé des remplacements, créé à la racine de la
// destination, relativement à son handle (NtCreateFile), avec l'ACL des administrateurs
// dès sa création ; les temporaires y naissent avec ces droits.
func (d *Dest) stagingDir() (string, error) {
	if d.staging != "" {
		return d.staging, nil
	}
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	name := ".kura-restore-" + hex.EncodeToString(rnd[:])
	top, err := d.root.Open(".")
	if err != nil {
		return "", err
	}
	defer top.Close()
	uname, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return "", err
	}
	oa := windows.OBJECT_ATTRIBUTES{
		RootDirectory:      windows.Handle(top.Fd()),
		ObjectName:         uname,
		Attributes:         windows.OBJ_CASE_INSENSITIVE,
		SecurityDescriptor: sddl(privateDirSDDL).sd,
	}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(&h, windows.SYNCHRONIZE|windows.FILE_LIST_DIRECTORY, &oa, &iosb, nil,
		windows.FILE_ATTRIBUTE_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0); err != nil {
		return "", fmt.Errorf("dossier de travail : %w", err)
	}
	windows.CloseHandle(h)
	d.staging = name
	return name, nil
}

// settle : un fichier remplacé dont l'ACL n'était pas protégée hérite de son dossier ;
// arrivé depuis le dossier de travail, son héritage est recalculé, par handle.
func (d *Dest) settle(rel string, r rights) error {
	if ctrl, _, err := r.sd.Control(); err == nil && ctrl&windows.SE_DACL_PROTECTED != 0 {
		return nil
	}
	f, err := d.root.Open(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	return applyRights(f, r)
}

// CreatePrivateDir crée un dossier réservé aux administrateurs et à SYSTEM dès sa
// création (descripteur passé à CreateDirectory) : rien de ce qu'il contiendra n'est
// lisible, même un instant, par un autre compte.
func CreatePrivateDir(p string) error {
	sd, err := windows.SecurityDescriptorFromString(privateDirSDDL)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	return windows.CreateDirectory(name, &sa)
}

// ownedBySelf : le propriétaire est le compte qui lance la restauration (administrateur).
func ownedBySelf(owner *windows.SID) bool {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && owner.Equals(u.User.Sid)
}

// SecureExistingDir rend privé un dossier --to existant (vide) : ouvert sans suivre de
// point de reparse, il doit appartenir aux Administrateurs, à SYSTEM ou au compte qui
// restaure (un autre propriétaire pourrait rouvrir ses droits), puis reçoit l'ACL privée par son handle.
func SecureExistingDir(p string) error {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return ErrLink
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) && !owner.IsWellKnown(windows.WinLocalSystemSid) && !ownedBySelf(owner) {
		return errNotAdminOwned
	}
	return applySD(h, sddl(privateDirSDDL))
}
