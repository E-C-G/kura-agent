package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

// maxBinarySize : un exécutable de l'agent extrait d'une archive (20 Mo aujourd'hui).
const maxBinarySize = 200 << 20

// ExtractZip extrait de l'archive Windows les fichiers names (noms seuls), rangés à la
// racine de l'archive ou dans son unique dossier kura-agent_<version>_windows_amd64 :
// rien d'autre n'est lu, aucun chemin de l'archive n'est suivi.
func ExtractZip(archive, prefix, dir string, names ...string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	want := wanted(prefix, names)
	found := map[string]bool{}
	for _, f := range zr.File {
		name, ok := want[f.Name]
		if !ok || f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > maxBinarySize {
			return fmt.Errorf("%s : %s trop volumineux", filepath.Base(archive), f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(filepath.Join(dir, name), rc)
		rc.Close()
		if err != nil {
			return err
		}
		found[name] = true
	}
	return missing(archive, names, found)
}

// ExtractTarGz : même règle pour l'archive macOS (.tar.gz).
func ExtractTarGz(archive, prefix, dir string, names ...string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	want := wanted(prefix, names)
	found := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, ok := want[path.Clean(h.Name)]
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxBinarySize {
			return fmt.Errorf("%s : %s trop volumineux", filepath.Base(archive), h.Name)
		}
		if err := writeFile(filepath.Join(dir, name), tr); err != nil {
			return err
		}
		found[name] = true
	}
	return missing(archive, names, found)
}

// wanted : chemin dans l'archive → nom du fichier extrait.
func wanted(prefix string, names []string) map[string]string {
	m := map[string]string{}
	for _, n := range names {
		m[n] = n
		if prefix != "" {
			m[prefix+"/"+n] = n
		}
	}
	return m
}

func missing(archive string, names []string, found map[string]bool) error {
	for _, n := range names {
		if !found[n] {
			return fmt.Errorf("%s : %s absent de l'archive", filepath.Base(archive), n)
		}
	}
	return nil
}

func writeFile(dst string, r io.Reader) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(r, maxBinarySize+1))
	if err == nil && n > maxBinarySize {
		err = fmt.Errorf("%s trop volumineux", filepath.Base(dst))
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
