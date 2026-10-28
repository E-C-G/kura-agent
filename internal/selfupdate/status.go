package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Status : fichier update.json du dossier de données, lu par kura-agent status et
// l'icône. Écrit par la vérification planifiée (Available, CheckedAt) et par
// self-update (Last, Journal).
type Status struct {
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	Available *Available `json:"available,omitempty"` // version plus récente vérifiée
	CheckErr  string     `json:"check_error,omitempty"`
	Last      *Record    `json:"last_update,omitempty"`
	// Journal : mise à jour en cours ; présent après une coupure (courant, arrêt
	// forcé), la commande suivante la termine ou la défait.
	Journal *Journal `json:"in_progress,omitempty"`
}

// Available décrit une version plus récente dont l'archive est attestée.
type Available struct {
	Version   string    `json:"version"`
	Published time.Time `json:"published"`
	NotesURL  string    `json:"notes_url,omitempty"`
}

// Record résume la dernière mise à jour (ou le dernier retour arrière).
type Record struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	At      time.Time `json:"at"`
	Mode    string    `json:"mode"`   // files | deb
	Result  string    `json:"result"` // ok | rolled_back | failed | rollback
	Message string    `json:"message,omitempty"`
}

// Journal suit une mise à jour en cours. Il ne désigne aucun chemin : les cibles et
// leurs sauvegardes se déduisent de l'installation.
type Journal struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Mode    string    `json:"mode"`
	Step    string    `json:"step"` // replacing | finishing
	Started time.Time `json:"started"`
}

// Modes de remplacement.
const (
	ModeFiles = "files" // renommage des exécutables (Windows, macOS, Linux hors paquet)
	ModeDeb   = "deb"   // paquet deb (dpkg -i)
)

// ReadStatus lit le fichier ; absent : zéro.
func ReadStatus(path string) (Status, error) {
	var s Status
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, fmt.Errorf("%s illisible : %w", path, err)
	}
	return s, nil
}

// CheckDue : la dernière vérification date de plus de 24 h (ou n'a jamais eu lieu).
func (s Status) CheckDue(now time.Time) bool {
	return s.CheckedAt == nil || now.Sub(*s.CheckedAt) >= 24*time.Hour || now.Before(*s.CheckedAt)
}
