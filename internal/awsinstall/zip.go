package awsinstall

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// maxUnzipped : la CLI décompressée fait environ 200 Mo.
const maxUnzipped = 1 << 30

// extractZip décompresse src dans dest, droits Unix conservés (scripts et
// exécutables). Un chemin qui sortirait de dest est refusé.
func extractZip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	inside := func(p string) bool { return p == root || strings.HasPrefix(p, root+string(filepath.Separator)) }
	var total int64
	for _, f := range r.File {
		name := filepath.FromSlash(f.Name)
		target := filepath.Join(root, name)
		if filepath.IsAbs(name) || !inside(target) {
			return fmt.Errorf("chemin refusé dans l'archive : %q", f.Name)
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		case mode&os.ModeSymlink != 0:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			link, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			l := string(link)
			if filepath.IsAbs(l) || !inside(filepath.Join(filepath.Dir(target), l)) {
				return fmt.Errorf("lien refusé dans l'archive : %q → %q", f.Name, l)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(l, target); err != nil {
				return err
			}
			continue
		case !mode.IsRegular():
			return fmt.Errorf("type de fichier refusé dans l'archive : %q", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm()&0o755|0o600)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxUnzipped-total+1))
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if total += n; total > maxUnzipped {
			return errors.New("archive anormalement grosse")
		}
	}
	return nil
}

// Emplacements de la CLI installée sous Linux (ceux de la documentation d'AWS).
const (
	linuxInstallDir = "/usr/local/aws-cli"
	linuxBinDir     = "/usr/local/bin"
)

// installZip décompresse le zip vérifié dans work, puis lance son installateur
// (aws/install) pour toute la machine.
func installZip(run func(logFile string, name string, args ...string) error) func(file, work, logFile string) (bool, error) {
	return func(file, work, logFile string) (bool, error) {
		dir := filepath.Join(work, "unpacked")
		if err := os.RemoveAll(dir); err != nil {
			return false, err
		}
		defer os.RemoveAll(dir)
		if err := extractZip(file, dir); err != nil {
			return false, fmt.Errorf("décompression : %w", err)
		}
		script := filepath.Join(dir, "aws", "install")
		if st, err := os.Stat(script); err != nil || !st.Mode().IsRegular() {
			return false, errors.New("aws/install absent de l'archive")
		}
		return false, run(logFile, script, "--install-dir", linuxInstallDir, "--bin-dir", linuxBinDir, "--update")
	}
}

// runLogged lance une commande, sortie dans logFile.
func runLogged(logFile, name string, args ...string) error {
	log, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s : %w", filepath.Base(name), err)
	}
	return nil
}
