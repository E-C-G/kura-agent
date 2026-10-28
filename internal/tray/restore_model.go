package tray

import (
	"fmt"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/ipc"
)

// Textes et calculs de la fenêtre de restauration, sans Win32 : testés partout.

// RestoreTitle : titre de la fenêtre.
const RestoreTitle = "Kura – Restaurer mes fichiers"

// FileRow : colonnes d'un fichier dans la liste (nom, taille, sauvegardé le, état).
func FileRow(f ipc.RestoreFile) [4]string {
	state := ""
	switch {
	case f.Problem != "":
		state = "non restaurable : " + f.Problem
	case f.DeletedSince != nil:
		state = "supprimé le " + f.DeletedSince.Local().Format("02/01/2006")
	case f.Newer:
		state = "modifié depuis"
	}
	return [4]string{strings.ReplaceAll(f.Rel, "/", `\`), HumanSize(f.Size), f.BackedUp.Local().Format("02/01/2006 15:04"), state}
}

// VersionRow : colonnes d'une version dans l'historique (date, taille, état).
func VersionRow(v ipc.RestoreVersion) [3]string {
	size, state := HumanSize(v.Size), ""
	switch {
	case v.Deleted:
		size, state = "—", "supprimé du poste"
	case v.Current:
		state = "version actuelle"
	}
	return [3]string{v.Date.Local().Format("02/01/2006 15:04"), size, state}
}

// StateAt : état demandé pour le jour choisi dans le calendrier (heure locale) ; nil
// pour aujourd'hui ou plus tard, c'est-à-dire la dernière sauvegarde.
func StateAt(day, now time.Time) *time.Time {
	y, m, d := day.Date()
	end := time.Date(y, m, d, 23, 59, 59, 0, now.Location())
	ny, nm, nd := now.Date()
	if !end.Before(time.Date(ny, nm, nd, 23, 59, 59, 0, now.Location())) {
		return nil
	}
	return &end
}

// ListSummary : ligne d'état après une liste.
func ListSummary(total int, bytes int64, at *time.Time) string {
	when := "à la dernière sauvegarde"
	if at != nil {
		when = "au " + at.Local().Format("02/01/2006")
	}
	if total == 0 {
		return "Aucun fichier sauvegardé dans ce dossier " + when + "."
	}
	return fmt.Sprintf("%d fichier(s), %s, %s : cocher ceux à restaurer.", total, HumanSize(bytes), when)
}

// ConfirmText : question posée avant de restaurer.
func ConfirmText(n int, bytes int64, folder, mode string) string {
	if mode == ipc.RestoreMissing {
		return fmt.Sprintf("Remettre à leur place, dans %s, ceux des %d fichier(s) cochés qui n'y sont plus (%s au plus) ?\n\n"+
			"Les fichiers présents ne sont pas touchés.", folder, n, HumanSize(bytes))
	}
	return fmt.Sprintf("Restaurer %d fichier(s) (%s) dans un nouveau dossier « Restauration Kura … » créé à côté de %s ?\n\n"+
		"Aucun fichier existant n'est remplacé.", n, HumanSize(bytes), folder)
}

// DoneText : titre et texte de la notification de fin.
func DoneText(ok bool, r ipc.RestoreData, msg string) (title, text string) {
	text = fmt.Sprintf("%d fichier(s) restauré(s), %s", r.Done, HumanSize(r.Bytes))
	if r.Kept > 0 {
		text += fmt.Sprintf(", %d déjà à leur place", r.Kept)
	}
	if r.Failed > 0 {
		text += fmt.Sprintf(", %d en échec (voir le journal)", r.Failed)
	}
	text += "."
	if msg != "" {
		text += " " + msg
	}
	title = "Restauration terminée"
	if !ok {
		title = "Restauration incomplète"
	}
	if r.Done == 0 && r.Kept == 0 && msg != "" && r.Failed == 0 {
		return "Restauration non faite", msg
	}
	return title, text
}

// HumanSize : taille lisible, à la française (1,2 Mo).
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d o", n)
	}
	v, i := float64(n)/unit, 0
	for v >= unit && i < 3 {
		v /= unit
		i++
	}
	return strings.Replace(fmt.Sprintf("%.1f %s", v, []string{"Ko", "Mo", "Go", "To"}[i]), ".", ",", 1)
}

// PickSize : volume des fichiers choisis.
func PickSize(files []ipc.RestoreFile, picked []int) int64 {
	var n int64
	for _, i := range picked {
		if i >= 0 && i < len(files) {
			n += files[i].Size
		}
	}
	return n
}
