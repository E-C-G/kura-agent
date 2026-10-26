package awsinstall

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// System renvoie l'installateur réel (Windows) ; ailleurs, Signature et RunMSI
// renvoient ErrUnsupported.
func System(out io.Writer, tempDir string) Installer {
	return Installer{
		Find:      Find,
		Signature: signature,
		Download:  HTTPDownload,
		RunMSI:    runMSI,
		Version:   Version,
		TempDir:   tempDir,
		Out:       out,
	}
}

// Find cherche aws dans le PATH, puis à l'emplacement d'installation par défaut.
func Find() (string, error) {
	if p, err := exec.LookPath("aws"); err == nil {
		return p, nil
	}
	if p := DefaultPath(); p != "" {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", errors.New("aws introuvable (ni dans le PATH, ni dans Program Files\\Amazon\\AWSCLIV2)")
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
