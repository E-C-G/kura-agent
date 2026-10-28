//go:build windows

package prepare

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// checkReclaimed : dir et son contenu appartiennent aux Administrateurs et aucune entrée
// ne nomme plus le service SQL Server.
func checkReclaimed(t *testing.T, dir, service string) {
	t.Helper()
	svc, _, _, err := windows.LookupSID("", `NT SERVICE\`+service)
	if err != nil {
		t.Fatal(err)
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil {
			t.Fatal(err)
		}
		if !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			t.Errorf("%s appartient à %s", p, owner)
		}
		if strings.Contains(sd.String(), svc.String()) {
			t.Errorf("%s : droits du service restés : %s", p, sd)
		}
		return nil
	})
}

func TestMSServiceName(t *testing.T) {
	for inst, want := range map[string]string{
		"localhost": "MSSQLSERVER", `localhost\SQLEXPRESS`: "MSSQL$SQLEXPRESS", `.\compta,1433`: "MSSQL$COMPTA",
		`(local)\MSSQLSERVER`: "MSSQLSERVER", "localhost,1433": "MSSQLSERVER",
	} {
		if got := msServiceName(inst); got != want {
			t.Errorf("%s → %s, attendu %s", inst, got, want)
		}
	}
}
