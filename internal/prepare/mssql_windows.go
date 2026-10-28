//go:build windows

package prepare

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// msWriteRights : modifier sans changer les droits ni le propriétaire (lecture, écriture,
// création, suppression de son propre fichier) ; ni WRITE_DAC ni WRITE_OWNER.
const msWriteRights = "0x1301bf"

// msGrant donne au compte du service SQL Server de instance le droit d'écrire dans dir,
// le dossier temporaire de la préparation, et à lui seul : son SID de service (NT
// SERVICE\MSSQL$…) et son compte de connexion s'il en a un propre. revoke rend dir et
// ce qu'il contient à l'agent seul : propriétaire Administrateurs, droits de la zone
// de préparation. Le service ne reçoit jamais rien sur la zone elle-même.
func msGrant(instance, dir string) (revoke func() error, err error) {
	sids, err := msServiceSIDs(msServiceName(instance))
	if err != nil {
		return nil, err
	}
	sddl := "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	for _, s := range sids {
		sddl += "(A;OICI;" + msWriteRights + ";;;" + s.String() + ")"
	}
	if err := setDACL(dir, sddl, nil); err != nil {
		return nil, fmt.Errorf("%s : %w", dir, err)
	}
	return func() error { return reclaim(dir) }, nil
}

// msServiceSIDs : SID du service, et celui de son compte de connexion quand ce n'est
// pas LocalSystem (déjà autorisé).
func msServiceSIDs(service string) ([]*windows.SID, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("gestionnaire de services : %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(service)
	if err != nil {
		return nil, fmt.Errorf("service %s introuvable (instance locale installée ?) : %w", service, err)
	}
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return nil, fmt.Errorf("configuration du service %s : %w", service, err)
	}
	svcSID, _, _, err := windows.LookupSID("", `NT SERVICE\`+service)
	if err != nil {
		return nil, fmt.Errorf("SID du service %s : %w", service, err)
	}
	sids := []*windows.SID{svcSID}
	account := strings.TrimPrefix(cfg.ServiceStartName, `.\`)
	if account != "" && !strings.EqualFold(account, "LocalSystem") {
		if sid, _, _, err := windows.LookupSID("", account); err == nil && !sid.Equals(svcSID) &&
			!sid.IsWellKnown(windows.WinLocalSystemSid) {
			sids = append(sids, sid)
		}
	}
	return sids, nil
}

// reclaim rend dir et son contenu à l'agent seul : propriétaire Administrateurs (le
// service ne garde pas la main sur les droits du fichier qu'il a écrit), droits protégés
// de la zone de préparation.
func reclaim(dir string) error {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		sddl := "D:PAI(A;;FA;;;SY)(A;;FA;;;BA)"
		if d.IsDir() {
			sddl = stagingSDDL
		}
		return setDACL(p, sddl, admins)
	})
	if err != nil {
		return err
	}
	return setDACL(dir, stagingSDDL, admins)
}

// setDACL pose une DACL protégée (et le propriétaire, s'il est donné) sur p.
func setDACL(p, sddl string, owner *windows.SID) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if owner != nil {
		info |= windows.OWNER_SECURITY_INFORMATION
	}
	return windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil)
}
