// Package state lit et écrit status.json et history.jsonl.
package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Résultats possibles d'un job ou d'une exécution, du meilleur au pire.
const (
	ResultOK        = "ok"
	ResultSkipped   = "skipped"
	ResultWarning   = "warning"
	ResultError     = "error"
	ResultGuard     = "guard"
	ResultCancelled = "cancelled"
)

var severity = map[string]int{
	ResultOK: 0, ResultSkipped: 1, ResultWarning: 1, ResultError: 2, ResultGuard: 3, ResultCancelled: 4,
}

// Worst renvoie le résultat le plus grave.
func Worst(results ...string) string {
	w := ResultOK
	for _, r := range results {
		if severity[r] > severity[w] {
			w = r
		}
	}
	return w
}

// Status est le contenu de status.json.
type Status struct {
	State       string     `json:"state"` // running | idle
	RunID       string     `json:"run_id"`
	Result      string     `json:"result,omitempty"`
	DryRun      bool       `json:"dry_run,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"` // tous les jobs configurés réussis
	// JobLastSuccess : dernière synchronisation réussie (ok ou avertissement) de chaque job.
	JobLastSuccess map[string]time.Time `json:"job_last_success,omitempty"`
	ExitCode       int                  `json:"exit_code"`
	Message        string               `json:"message,omitempty"`
	PreTasks       []PreTaskStatus      `json:"pre_tasks,omitempty"`
	Prepares       []PrepareStatus      `json:"prepares,omitempty"`
	Jobs           []JobStatus          `json:"jobs"`
	// Current est le job en cours pendant une exécution (info-bulle de l'icône).
	Current *Progress `json:"current,omitempty"`
}

// Progress situe le job en cours.
type Progress struct {
	Job   string `json:"job"`   // nom du job
	Index int    `json:"index"` // 1..Total
	Total int    `json:"total"`
}

// PreTaskStatus résume une pré-tâche.
type PreTaskStatus struct {
	Process   string `json:"process"`
	Found     int    `json:"found"`  // instances trouvées
	Closed    int    `json:"closed"` // instances arrêtées
	Result    string `json:"result"`
	Sessions  []int  `json:"sessions,omitempty"`
	Graceful  int    `json:"graceful,omitempty"`  // instances fermées par l'icône (fermeture douce)
	Postponed int    `json:"postponed,omitempty"` // reports accordés à l'utilisateur
	Message   string `json:"message,omitempty"`
}

// PrepareStatus résume une préparation (archive, export) faite avant les jobs.
type PrepareStatus struct {
	ID        string  `json:"id"`
	Type      string  `json:"type"`
	Result    string  `json:"result"`
	Files     int     `json:"files,omitempty"`   // fichiers archivés
	Missing   int     `json:"missing,omitempty"` // fichiers illisibles laissés de côté (on_locked: skip)
	Size      int64   `json:"size,omitempty"`    // octets produits
	DurationS float64 `json:"duration_s"`
	Message   string  `json:"message,omitempty"`
}

// JobStatus résume un job.
type JobStatus struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Result    string  `json:"result"`
	Uploaded  int     `json:"uploaded"`
	Deleted   int     `json:"deleted"`
	Skipped   int     `json:"skipped,omitempty"`
	ExitCode  int     `json:"exit_code"`
	Attempts  int     `json:"attempts"`
	DurationS float64 `json:"duration_s"`
	Message   string  `json:"message,omitempty"`
}

// WriteAtomic écrit v en JSON dans path sans jamais laisser un fichier à moitié écrit :
// fichier temporaire dans le même dossier, synchronisé, puis renommé.
func WriteAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".status-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // sans effet après le renommage
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	// Lisible par tous (CreateTemp crée en 0600) : l'icône et kura-agent status
	// tournent sous le compte de l'utilisateur ; aucun secret ici. Sous Windows,
	// seuls les droits du dossier comptent.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return rename(tmp.Name(), path)
}

// Read lit status.json ; un fichier absent renvoie (nil, nil).
func Read(path string) (*Status, error) {
	data, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s illisible : %w", path, err)
	}
	return &s, nil
}

// HistoryLimit est le nombre d'exécutions gardées dans history.jsonl.
const HistoryLimit = 500

// AppendHistory ajoute une exécution à l'historique et le borne à HistoryLimit lignes.
func AppendHistory(path string, s Status) error {
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	var lines [][]byte
	if data, err := readFile(path); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			if l := bytes.TrimSpace(sc.Bytes()); len(l) > 0 {
				lines = append(lines, append([]byte(nil), l...))
			}
		}
	}
	lines = append(lines, line)
	if len(lines) > HistoryLimit {
		lines = lines[len(lines)-HistoryLimit:]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(bytes.Join(lines, []byte("\n")), '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil { // lisible par tous, comme status.json
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return rename(tmp.Name(), path)
}
