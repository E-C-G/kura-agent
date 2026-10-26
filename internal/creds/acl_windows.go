//go:build windows

package creds

import "golang.org/x/sys/windows"

// dirSDDL : SYSTEM et Administrateurs seuls, ACL protégée (rien n'est hérité de
// C:\ProgramData, où les Utilisateurs peuvent lire et créer).
const dirSDDL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

func secureDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString(dirSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
