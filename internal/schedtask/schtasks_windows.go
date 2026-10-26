//go:build windows

package schedtask

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func schtasks(args ...string) ([]byte, error) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return exec.Command(filepath.Join(root, "System32", "schtasks.exe"), args...).CombinedOutput()
}

// Register crée ou remplace la tâche à partir de son XML.
func Register(def string) error {
	f, err := os.CreateTemp("", "kura-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(UTF16(def)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if out, err := schtasks("/Create", "/TN", Name, "/XML", f.Name(), "/F"); err != nil {
		return fmt.Errorf("schtasks /Create : %v : %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Exists indique si la tâche est enregistrée.
func Exists() bool {
	_, err := schtasks("/Query", "/TN", Name)
	return err == nil
}

// Delete supprime la tâche ; absente n'est pas une erreur.
func Delete() error {
	if !Exists() {
		return nil
	}
	if out, err := schtasks("/Delete", "/TN", Name, "/F"); err != nil {
		return fmt.Errorf("schtasks /Delete : %v : %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
