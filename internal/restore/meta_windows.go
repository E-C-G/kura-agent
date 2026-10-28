package restore

import "golang.org/x/sys/windows"

// privateSDDL : SYSTEM et Administrateurs, sans héritage du dossier.
const privateSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"

// setPrivate réserve le fichier aux administrateurs et à SYSTEM.
func setPrivate(p string) error {
	sd, err := windows.SecurityDescriptorFromString(privateSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// copySecurity donne au fichier to l'ACL de from : ses entrées explicites, et sa
// protection contre l'héritage si elle en a une (fichier plus restreint que son dossier).
func copySecurity(from, to string) error {
	sd, err := windows.GetNamedSecurityInfo(from, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		return err
	}
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if ctrl&windows.SE_DACL_PROTECTED != 0 {
		flags |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		flags |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	return windows.SetNamedSecurityInfo(to, windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil)
}
