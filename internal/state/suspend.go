package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Suspension est le contenu de suspend.json : tant qu'elle court, les exécutions
// planifiées (run --scheduled) se terminent aussitôt sans rien faire.
type Suspension struct {
	Until time.Time `json:"until"`
	By    string    `json:"by,omitempty"`
	At    time.Time `json:"at"`
}

// Active indique si la suspension court encore.
func (s *Suspension) Active(now time.Time) bool { return s != nil && now.Before(s.Until) }

// ReadSuspension relit suspend.json ; nil s'il n'existe pas.
func ReadSuspension(path string) (*Suspension, error) {
	data, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Suspension
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s illisible : %w", path, err)
	}
	return &s, nil
}

// WriteSuspension enregistre une suspension (écriture atomique).
func WriteSuspension(path string, s Suspension) error {
	return WriteAtomic(path, s)
}

// ClearSuspension reprend la planification ; absente n'est pas une erreur.
func ClearSuspension(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
