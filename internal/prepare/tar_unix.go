//go:build !windows

package prepare

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// tarTree écrit dans tw le contenu du dossier dir : la racine (./) puis chaque entrée,
// dans l'ordre des noms, ouverte sous dir (os.Root) sans jamais suivre de lien.
// Propriétaires numériques (ceux du conteneur), modes (bits spéciaux compris), dates ;
// liens symboliques gardés comme liens, liens physiques comme liens vers la première
// entrée ; tubes, sockets et périphériques laissés de côté.
func tarTree(ctx context.Context, dir string, excluded func(string, bool) bool, tw *tar.Writer) (tarStats, error) {
	var st tarStats
	before, err := os.Lstat(dir)
	if err != nil {
		return st, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return st, err
	}
	defer root.Close()
	if now, err := root.Stat("."); err != nil || !os.SameFile(before, now) {
		return st, fmt.Errorf("%s a changé pendant l'ouverture (remplacé par un lien ?)", dir)
	}
	type inode struct{ dev, ino uint64 }
	seen := map[inode]string{}
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("%s : %w", rel, err)
		}
		if rel != "." && excluded(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		fi, err := root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("%s : %w", rel, err)
		}
		sys, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s : propriétaire illisible", rel)
		}
		hdr := &tar.Header{
			Name: rel, Mode: int64(sys.Mode & 0o7777), Uid: int(sys.Uid), Gid: int(sys.Gid),
			ModTime: fi.ModTime(), Format: tar.FormatPAX,
		}
		switch m := fi.Mode(); {
		case m.IsDir():
			hdr.Typeflag = tar.TypeDir
			if rel == "." {
				hdr.Name = "./"
			} else {
				hdr.Name = rel + "/"
			}
		case m&os.ModeSymlink != 0:
			// La cible est gardée telle quelle, jamais suivie : lue par le chemin, juste
			// après l'avoir vu lien sous la racine (seul son texte entre dans l'archive).
			target, err := os.Readlink(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				return fmt.Errorf("%s : %w", rel, err)
			}
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, target
			st.links++
		case m.IsRegular():
			key := inode{uint64(sys.Dev), uint64(sys.Ino)}
			if first, ok := seen[key]; ok && sys.Nlink > 1 {
				hdr.Typeflag, hdr.Linkname = tar.TypeLink, first
				st.links++
				break
			}
			seen[key] = rel
			return st.addFile(ctx, root, rel, hdr, tw)
		default:
			st.skipped++ // tube, socket, périphérique
			return nil
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		st.entries++
		return nil
	})
	return st, err
}

// addFile écrit un fichier ordinaire : la taille vue à l'ouverture fait foi ; un fichier
// qui change pendant la lecture (copie sans gel) est tronqué ou complété de zéros, et noté.
func (st *tarStats) addFile(ctx context.Context, root *os.Root, rel string, hdr *tar.Header, tw *tar.Writer) error {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("%s : %w", rel, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%s : %w", rel, err)
	}
	if !fi.Mode().IsRegular() {
		st.skipped++
		return nil
	}
	hdr.Typeflag, hdr.Size, hdr.ModTime = tar.TypeReg, fi.Size(), fi.ModTime()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := copyN(ctx, tw, f, fi.Size())
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s : %w", rel, err)
	}
	changed := n < fi.Size()
	if changed {
		if _, err := copyN(ctx, tw, zeros{}, fi.Size()-n); err != nil {
			return err
		}
	}
	if after, err := f.Stat(); err == nil && (after.Size() != fi.Size() || !after.ModTime().Equal(fi.ModTime())) {
		changed = true
	}
	if changed {
		st.changed = append(st.changed, rel)
	}
	st.entries++
	st.bytes += fi.Size()
	return nil
}

// copyN copie n octets de r vers w, par blocs, en s'arrêtant si ctx est annulé.
func copyN(ctx context.Context, w io.Writer, r io.Reader, n int64) (int64, error) {
	buf := make([]byte, 1<<20)
	var done int64
	for done < n {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		chunk := buf
		if rest := n - done; rest < int64(len(chunk)) {
			chunk = chunk[:rest]
		}
		k, rerr := r.Read(chunk)
		if k > 0 {
			if _, err := w.Write(chunk[:k]); err != nil {
				return done, err
			}
			done += int64(k)
		}
		if rerr != nil {
			return done, rerr
		}
	}
	return done, nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
