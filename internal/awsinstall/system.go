package awsinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// System renvoie l'installateur réel de ce système (SystemPackage).
func System(out io.Writer, tempDir string) Installer {
	return Installer{
		Find:     Find,
		Download: HTTPDownload,
		Version:  Version,
		Package:  SystemPackage(),
		TempDir:  tempDir,
		Out:      out,
	}
}

// Find cherche aws dans le PATH (sous macOS, dans les seuls dossiers du PATH du démon,
// MacSearchDirs), puis à l'emplacement d'installation par défaut. Sous Linux, la CLI du paquet snap est écartée : snap-confine exige des droits que le
// service kura-sync n'a pas (il ne garde que la lecture du disque), elle échoue donc
// sous le service alors qu'elle marche avec sudo.
func Find() (string, error) {
	snap := ""
	switch runtime.GOOS {
	case "darwin":
		for _, dir := range MacSearchDirs {
			if p, err := exec.LookPath(filepath.Join(dir, "aws")); err == nil {
				return p, nil
			}
		}
	case "linux":
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			if dir == "" {
				continue
			}
			p, err := exec.LookPath(filepath.Join(dir, "aws"))
			if err != nil {
				continue
			}
			if IsSnap(p) {
				snap = p
				continue
			}
			return p, nil
		}
	default:
		if p, err := exec.LookPath("aws"); err == nil {
			return p, nil
		}
	}
	if p := DefaultPath(); p != "" {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	if snap != "" {
		return "", fmt.Errorf("seule la CLI AWS du paquet snap est présente (%s), inutilisable sous le service : sudo kura-agent install installe la version officielle", snap)
	}
	return "", fmt.Errorf("aws introuvable (ni dans le PATH, ni en %s)", orUnknown(DefaultPath()))
}

// IsSnap : CLI fournie par un paquet snap (Linux).
func IsSnap(p string) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return isSnapPath(p, filepath.EvalSymlinks)
}

func isSnapPath(p string, eval func(string) (string, error)) bool {
	if strings.HasPrefix(p, "/snap/") {
		return true
	}
	r, err := eval(p)
	return err == nil && (strings.HasPrefix(r, "/snap/") || strings.HasSuffix(r, "/usr/bin/snap"))
}

// Version renvoie la première ligne de `aws --version` (aws-cli/2.x.y …).
func Version(aws string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, aws, "--version").CombinedOutput()
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if err != nil {
		return "", errors.New(strings.TrimSpace(err.Error() + " " + line))
	}
	if !strings.HasPrefix(line, "aws-cli/") {
		return "", errors.New("réponse inattendue : " + line)
	}
	return strings.TrimSpace(line), nil
}

// parseSignatureOutput lit la sortie de PowerShell : état, puis sujet du signataire.
func parseSignatureOutput(out string) (status, subject string, err error) {
	out = strings.ReplaceAll(strings.TrimPrefix(out, "\ufeff"), "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	status = strings.TrimSpace(strings.TrimPrefix(lines[0], "\ufeff"))
	if len(lines) > 1 {
		subject = strings.TrimSpace(lines[1])
	}
	if status == "" {
		return "", "", errors.New("powershell : réponse vide")
	}
	return status, subject, nil
}

func orUnknown(p string) string {
	if p == "" {
		return "emplacement par défaut inconnu"
	}
	return p
}
