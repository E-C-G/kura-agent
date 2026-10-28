package selfupdate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Remplacement des exécutables par renommage, dans leur propre dossier :
//
//	cible        C:\Program Files\Kura\kura-agent.exe (ou /usr/local/bin/kura-agent)
//	sauvegarde   cible.old : version précédente, gardée jusqu'à la mise à jour suivante
//	nouveau      fichier vérifié, copié à côté de la cible puis renommé
//
// Windows permet de renommer un exécutable en cours d'exécution (l'icône, ou la
// commande elle-même), pas de le supprimer ni de l'écraser ; Linux et macOS remplacent
// sans difficulté. Les noms de sauvegarde se déduisent de la cible : un journal n'a
// jamais à désigner un chemin.

// BackupSuffix : suffixe de la version précédente.
const BackupSuffix = ".old"

// File est un exécutable à remplacer.
type File struct {
	Target string // chemin installé
	New    string // nouvelle version vérifiée (même volume que Target sous Windows)
}

// Swap remplace chaque cible par sa nouvelle version, la précédente gardée en
// cible.old. Une erreur en cours de route remet les cibles déjà remplacées.
func Swap(files []File) error {
	var done []string
	for _, f := range files {
		if err := swapOne(f); err != nil {
			if rerr := Restore(done); rerr != nil {
				return fmt.Errorf("%v ; retour arrière incomplet : %v", err, rerr)
			}
			return err
		}
		done = append(done, f.Target)
	}
	return nil
}

func swapOne(f File) error {
	if _, err := os.Stat(f.Target); err != nil {
		return fmt.Errorf("%s : %w", f.Target, err)
	}
	backup := f.Target + BackupSuffix
	if err := clearBackup(backup); err != nil {
		return err
	}
	// La nouvelle version est d'abord copiée à côté de la cible : le renommage final
	// reste dans un même dossier, donc un même volume.
	staged := f.Target + ".new"
	_ = os.Remove(staged)
	if err := copyFile(f.New, staged); err != nil {
		return fmt.Errorf("copie de %s : %w", filepath.Base(f.Target), err)
	}
	if err := os.Rename(f.Target, backup); err != nil {
		os.Remove(staged)
		return fmt.Errorf("mise de côté de %s : %w", f.Target, err)
	}
	if err := os.Rename(staged, f.Target); err != nil {
		_ = os.Rename(backup, f.Target)
		os.Remove(staged)
		return fmt.Errorf("remplacement de %s : %w", f.Target, err)
	}
	return nil
}

// clearBackup supprime une ancienne sauvegarde. Sous Windows, celle d'une mise à jour
// précédente peut encore tourner (icône ouverte depuis) : elle est alors renommée à
// l'écart et supprimée plus tard (CleanBackups).
func clearBackup(backup string) error {
	err := os.Remove(backup)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	aside := backup + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if rerr := os.Rename(backup, aside); rerr != nil {
		return fmt.Errorf("ancienne sauvegarde %s ni supprimée (%v) ni écartée (%v)", backup, err, rerr)
	}
	return nil
}

// Restore remet la version précédente de chaque cible (cible.old → cible). La version
// remplacée est écartée (cible.failed) puis supprimée si possible.
func Restore(targets []string) error {
	var errs []error
	for _, t := range targets {
		backup := t + BackupSuffix
		if _, err := os.Stat(backup); err != nil {
			errs = append(errs, fmt.Errorf("%s : version précédente absente", t))
			continue
		}
		failed := t + ".failed"
		_ = clearBackup(failed)
		if _, err := os.Stat(t); err == nil {
			if err := os.Rename(t, failed); err != nil {
				errs = append(errs, fmt.Errorf("%s : %w", t, err))
				continue
			}
		}
		if err := os.Rename(backup, t); err != nil {
			_ = os.Rename(failed, t)
			errs = append(errs, fmt.Errorf("%s : %w", t, err))
			continue
		}
		_ = os.Remove(failed)
	}
	return errors.Join(errs...)
}

// CleanBackups supprime les sauvegardes écartées (cible.old-*, cible.failed-*) que plus
// rien n'utilise ; celles encore ouvertes restent pour la fois suivante.
func CleanBackups(targets []string) {
	for _, t := range targets {
		for _, pat := range []string{t + BackupSuffix + "-*", t + ".failed*", t + ".new"} {
			m, _ := filepath.Glob(pat)
			for _, p := range m {
				_ = os.Remove(p)
			}
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
