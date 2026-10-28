package ipc

import (
	"encoding/json"
	"time"
)

// Restauration depuis l'icône : la fenêtre (icône, compte de la session) sert un canal
// à part, publié pendant qu'elle est ouverte ; la tâche \Kura\Restore (SYSTEM) s'y
// connecte, lit le bucket et n'écrit qu'au nom du compte de l'icône.
//
//	tâche → icône  restore_hello     Restore.Account, Restore.Roots, Restore.Days ; Message si refus
//	icône → tâche  restore_list      Path, Restore.At → restore_files (lots, Restore.Req) puis réponse restore_listed
//	icône → tâche  restore_history   Restore.Rel → réponse restore_versions
//	icône → tâche  restore_pick      Restore.Rels (lots) : fichiers choisis de la dernière liste
//	icône → tâche  restore_run       Restore.Mode → restore_progress (événements) puis réponse restore_done
//
// Toute vérification se fait côté tâche : l'icône n'est qu'une interface.
const (
	TypeRestoreHello    = "restore_hello"
	TypeRestoreList     = "restore_list"
	TypeRestoreFiles    = "restore_files"
	TypeRestoreListed   = "restore_listed"
	TypeRestoreHistory  = "restore_history"
	TypeRestoreVersions = "restore_versions"
	TypeRestorePick     = "restore_pick"
	TypeRestoreRun      = "restore_run"
	TypeRestoreProgress = "restore_progress"
	TypeRestoreDone     = "restore_done"
)

// Modes de restauration depuis l'icône.
const (
	RestoreCopy    = "copy"    // dans un nouveau dossier « Restauration Kura … » à côté du dossier choisi
	RestoreMissing = "missing" // à leur place, seulement les fichiers absents
)

// RestoreData : champs des messages restore_*.
type RestoreData struct {
	Account string   `json:"account,omitempty"`
	Roots   []string `json:"roots,omitempty"` // dossiers sauvegardés que le compte peut restaurer
	Days    int      `json:"days,omitempty"`  // fenêtre des versions anciennes

	At   *time.Time `json:"at,omitempty"` // état demandé ; absent : dernière sauvegarde
	Mode string     `json:"mode,omitempty"`
	Req  int        `json:"req,omitempty"` // requête à laquelle se rattache un lot

	Files    []RestoreFile    `json:"files,omitempty"`
	Rel      string           `json:"rel,omitempty"`
	Rels     []string         `json:"rels,omitempty"`
	Versions []RestoreVersion `json:"versions,omitempty"`

	Done   int    `json:"done,omitempty"`
	Total  int    `json:"total,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Failed int    `json:"failed,omitempty"`
	Kept   int    `json:"kept,omitempty"` // laissés : déjà présents (à leur place)
	Dir    string `json:"dir,omitempty"`  // dossier qui a reçu les fichiers
	Log    string `json:"log,omitempty"`
}

// RestoreFile : un fichier dans l'état demandé.
type RestoreFile struct {
	Rel          string     `json:"rel"` // chemin sous le dossier choisi, séparateurs /
	Size         int64      `json:"size"`
	BackedUp     time.Time  `json:"backed_up"`
	DeletedSince *time.Time `json:"deleted_since,omitempty"`
	Newer        bool       `json:"newer,omitempty"`
	Problem      string     `json:"problem,omitempty"`
}

// RestoreVersion : une version d'un fichier.
type RestoreVersion struct {
	Date    time.Time `json:"date"`
	Size    int64     `json:"size"`
	Deleted bool      `json:"deleted,omitempty"`
	Current bool      `json:"current,omitempty"`
}

// chunkBytes : taille visée d'un lot, loin sous maxLine (une ligne de canal).
const chunkBytes = 32 << 10

// ChunkFiles découpe une liste en lots qui tiennent chacun dans un message.
func ChunkFiles(files []RestoreFile) [][]RestoreFile {
	var out [][]RestoreFile
	var cur []RestoreFile
	size := 0
	for _, f := range files {
		b, _ := json.Marshal(f)
		if len(cur) > 0 && size+len(b) > chunkBytes {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, f)
		size += len(b) + 1
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// ChunkStrings découpe une liste de chemins en lots.
func ChunkStrings(list []string) [][]string {
	var out [][]string
	var cur []string
	size := 0
	for _, s := range list {
		n := len(s) + 8
		if len(cur) > 0 && size+n > chunkBytes {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, s)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}
