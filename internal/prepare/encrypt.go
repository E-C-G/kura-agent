package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/E-C-G/kura-agent/internal/guard"
	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Chiffrement (gpg, age) : le résultat d'une préparation qui précède (input), ou un fichier
// ou dossier du poste (source), est chiffré pour des clés publiques ; le bucket ne reçoit
// que le chiffré. L'agent ne détient jamais de clé privée.

// sealer : un format de chiffrement.
type sealer interface {
	ext() string
	// file chiffre r dans dst (nouveau fichier) ; name : nom du clair, dont l'extension
	// dit s'il est déjà compressé.
	file(ctx context.Context, r io.Reader, dst, name string) error
}

// runAge chiffre avec age, dans l'agent (filippo.io/age). Un export de base passe
// directement dans le chiffrement : il n'existe jamais en clair sur le disque.
func runAge(ctx context.Context, cfg *config.Config, p config.Prepare, d Deps, tmp string, ps *state.PrepareStatus, log *slog.Logger) error {
	var recips []age.Recipient
	for _, r := range cfg.RecipientsOf(p) {
		rec, _, err := config.ParseAgeRecipient(r.Age)
		if err != nil {
			return fmt.Errorf("recipients : %w", err)
		}
		recips = append(recips, rec)
		ps.Recipients = append(ps.Recipients, strings.TrimSpace(r.Age))
	}
	if len(recips) == 0 {
		return errors.New("aucun destinataire age")
	}
	log.Info("destinataires age", "clés", ps.Recipients)
	seal := func(w io.Writer) (io.WriteCloser, error) { return age.Encrypt(w, recips...) }
	if in, ok := cfg.PrepareByID(p.Input); ok && in.Type == config.PrepareDBExport {
		return runDB(ctx, cfg, in, d, tmp, ps, log, seal)
	}
	if in, ok := cfg.PrepareByID(p.Input); ok && in.Type == config.PrepareDocker {
		// Archive chiffrée pendant la lecture : jamais en clair dans la zone.
		return runDocker(ctx, cfg, in, d, tmp, ps, log, seal)
	}
	return encryptAll(ctx, p, d, tmp, ps, &ageSealer{seal: seal})
}

type ageSealer struct {
	seal func(io.Writer) (io.WriteCloser, error)
}

func (a *ageSealer) ext() string { return ".age" }

func (a *ageSealer) file(ctx context.Context, r io.Reader, dst, _ string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	w, err := a.seal(f)
	if err == nil {
		_, err = copyCtx(ctx, w, r)
		err = errors.Join(err, w.Close())
	}
	if err = errors.Join(err, f.Close()); err != nil {
		os.Remove(dst)
	}
	return err
}

// encryptAll chiffre l'entrée de p (résultat d'une préparation, effacé ensuite, réussi
// ou non) ou sa source, fichier par fichier, dans tmp.
func encryptAll(ctx context.Context, p config.Prepare, d Deps, tmp string, ps *state.PrepareStatus, s sealer) error {
	if p.Input != "" {
		src := filepath.Join(d.StagingDir, p.Input)
		defer os.RemoveAll(src) // le clair ne reste pas
		if _, err := os.Lstat(src); err != nil {
			return fmt.Errorf("résultat de la préparation %s absent : %w", p.Input, err)
		}
		return sealTree(ctx, src, nil, tmp, ps, s)
	}
	return sealTree(ctx, p.Source, d.AgentDirs, tmp, ps, s)
}

// sealTree chiffre src (fichier ou dossier) dans dst, en gardant l'arborescence : chaque
// fichier est ouvert sous src (os.Root), sans jamais suivre de lien ni de jonction ;
// dossiers de l'agent laissés de côté. Un fichier illisible fait échouer la préparation.
func sealTree(ctx context.Context, src string, agentPaths []string, dst string, ps *state.PrepareStatus, s sealer) error {
	if err := guard.CheckSourceNotLink(src); err != nil {
		return err
	}
	agent, agentNames := agentFiles(agentPaths)
	if err := refuseAgentSource(src, agent); err != nil {
		return err
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	var missing []string
	one := func(root *os.Root, rel string) error {
		n, err := sealOne(ctx, root, rel, dst, s)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			missing = append(missing, fmt.Sprintf("%s (%v)", rel, err))
		default:
			ps.Files++
			ps.Size += n
		}
		return nil
	}
	if fi.Mode().IsRegular() {
		root, err := os.OpenRoot(filepath.Dir(src))
		if err != nil {
			return err
		}
		defer root.Close()
		if err := one(root, filepath.Base(src)); err != nil {
			return err
		}
	} else {
		root, err := os.OpenRoot(src)
		if err != nil {
			return err
		}
		defer root.Close()
		if now, err := root.Stat("."); err != nil || !os.SameFile(fi, now) {
			return fmt.Errorf("%s a changé pendant l'ouverture (remplacé par un lien ?)", src)
		}
		err = fs.WalkDir(root.FS(), ".", func(rel string, de fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if rel == "." {
					return err
				}
				missing = append(missing, rel+"/")
				if de != nil && de.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if rel == "." {
				return nil
			}
			skip := func() error {
				if de.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if agentNames[strings.ToLower(de.Name())] {
				if fi, err := os.Stat(filepath.Join(src, filepath.FromSlash(rel))); err == nil && sameAny(fi, agent) {
					return skip()
				}
			}
			t := de.Type()
			if t&fs.ModeSymlink != 0 || t&fs.ModeIrregular != 0 && de.IsDir() {
				return skip() // lien ou jonction : jamais suivi
			}
			if de.IsDir() || t&fs.ModeIrregular == 0 && !t.IsRegular() {
				return nil
			}
			return one(root, rel)
		})
		if err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d fichier(s) non chiffré(s) : %s", len(missing), sample(missing))
	}
	if ps.Files == 0 {
		return fmt.Errorf("rien à chiffrer dans %s", src)
	}
	return nil
}

// sealOne chiffre rel (ouvert sous root) vers dst/rel<ext> ; renvoie la taille du chiffré.
func sealOne(ctx context.Context, root *os.Root, rel, dst string, s sealer) (int64, error) {
	in, err := root.OpenFile(rel, os.O_RDONLY|openNoBlock, 0)
	if err != nil {
		if strings.Contains(err.Error(), "escapes") {
			return 0, errEscapes
		}
		return 0, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, errNotRegular
	}
	target := filepath.Join(dst, filepath.FromSlash(rel)) + s.ext()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return 0, err
	}
	if err := s.file(ctx, in, target, rel); err != nil {
		os.Remove(target)
		return 0, err
	}
	st, err := os.Stat(target)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// compressed : le fichier est déjà compressé (gpg ne le recompresse pas).
func compressed(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".dump", ".gz", ".zip", ".7z", ".xz", ".bz2", ".zst", ".age", ".gpg", ".jpg", ".png", ".mp4":
		return true
	}
	return false
}
