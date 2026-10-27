//go:build windows

package secdir

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const sddl = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

func owner(t *testing.T, dir string) *windows.SID {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	o, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("%s : DACL non protégée", dir)
	}
	return o
}

func TestSecureNewAndExisting(t *testing.T) {
	base := t.TempDir()
	for _, dir := range []string{filepath.Join(base, "neuf", "Kura"), base} {
		if err := Secure(dir, sddl); err != nil {
			t.Fatalf("%s : %v", dir, err)
		}
		if o := owner(t, dir); !o.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			t.Fatalf("%s : propriétaire %s, Administrateurs attendu", dir, o)
		}
	}
	// Relancé (réinstallation) : accepté.
	if err := Secure(base, sddl); err != nil {
		t.Fatal(err)
	}
}

func TestSecureRefusesJunction(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "cible")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "Kura")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink : %v %s", err, out)
	}
	err := Secure(link, sddl)
	if !IsReparsePoint(err) || !strings.Contains(err.Error(), "jonction") {
		t.Fatalf("jonction acceptée : %v", err)
	}
}

func TestTrustedOwner(t *testing.T) {
	for _, c := range []struct {
		sid  string
		want bool
	}{
		{"S-1-5-18", true},      // SYSTEM
		{"S-1-5-32-544", true},  // Administrateurs
		{"S-1-5-32-545", false}, // Utilisateurs
		{"S-1-1-0", false},      // Tout le monde
		{"S-1-5-11", false},     // Utilisateurs authentifiés
	} {
		s, err := windows.StringToSid(c.sid)
		if err != nil {
			t.Fatal(err)
		}
		if got := trustedOwner(s); got != c.want {
			t.Errorf("%s : %v", c.sid, got)
		}
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if !trustedOwner(u.User.Sid) {
		t.Fatal("compte qui installe refusé")
	}
}

func TestErrorsMessages(t *testing.T) {
	var e error = &ForeignOwnerError{Dir: `C:\ProgramData\Kura`, Owner: `PC\bob`}
	if !strings.Contains(e.Error(), "takeown") || !strings.Contains(e.Error(), `PC\bob`) {
		t.Fatal(e)
	}
	if !errors.As(e, new(*ForeignOwnerError)) {
		t.Fatal("type")
	}
}
