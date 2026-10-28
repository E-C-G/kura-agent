//go:build windows

package schedtask

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const cpOEM = 1 // CP_OEMCP : page de code des messages de schtasks (850 en français)

func schtasks(args ...string) (string, error) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	cmd := exec.Command(filepath.Join(root, "System32", "schtasks.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(oem(out)), err
}

// oem convertit la sortie de schtasks (page de code OEM) en UTF-8 : sans cela,
// « Accès refusé » s'affiche « Acc�s refus� ».
func oem(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	n, err := windows.MultiByteToWideChar(cpOEM, 0, &b[0], int32(len(b)), nil, 0)
	if err != nil || n <= 0 {
		return string(b)
	}
	u := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(cpOEM, 0, &b[0], int32(len(b)), &u[0], n); err != nil {
		return string(b)
	}
	return windows.UTF16ToString(u)
}

func create(name, def string) (string, error) {
	f, err := os.CreateTemp("", "kura-task-*.xml")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(UTF16(def)); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return schtasks("/Create", "/TN", name, "/XML", f.Name(), "/F")
}

// Register crée ou remplace la tâche à partir de son XML. Si schtasks refuse le
// descripteur de sécurité (droit de lancement des comptes de allow_run_now), la tâche
// est créée sans, et l'erreur renvoyée est un *RunNowNotAppliedError.
func Register(def string) error { return RegisterNamed(Name, def) }

// RegisterNamed crée ou remplace la tâche name (voir Register).
func RegisterNamed(name, def string) error {
	out, err := create(name, def)
	if err == nil {
		return nil
	}
	first := fmt.Sprintf("%v : %s", err, out)
	if bare := WithoutSecurityDescriptor(def); bare != def {
		if out2, err2 := create(name, bare); err2 == nil {
			return &RunNowNotAppliedError{Cause: first}
		} else {
			return fmt.Errorf("schtasks /Create : %s ; sans descripteur de sécurité : %v : %s", first, err2, out2)
		}
	}
	return fmt.Errorf("schtasks /Create : %s", first)
}

// Exists indique si la tâche est enregistrée.
func Exists() bool { return ExistsNamed(Name) }

// ExistsNamed indique si la tâche name est enregistrée.
func ExistsNamed(name string) bool {
	_, err := schtasks("/Query", "/TN", name)
	return err == nil
}

// Delete supprime la tâche ; absente n'est pas une erreur.
func Delete() error { return DeleteNamed(Name) }

// DeleteNamed supprime la tâche name ; absente n'est pas une erreur.
func DeleteNamed(name string) error {
	if !ExistsNamed(name) {
		return nil
	}
	if out, err := schtasks("/Delete", "/TN", name, "/F"); err != nil {
		return fmt.Errorf("schtasks /Delete %s : %v : %s", name, err, out)
	}
	return nil
}
