package ipc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestChunks(t *testing.T) {
	var files []RestoreFile
	for i := 0; i < 2000; i++ {
		files = append(files, RestoreFile{Rel: strings.Repeat("dossier/", 20) + "fichier.txt", Size: int64(i), BackedUp: time.Now()})
	}
	chunks := ChunkFiles(files)
	n := 0
	for _, c := range chunks {
		b, _ := json.Marshal(Msg{Type: TypeRestoreFiles, Restore: &RestoreData{Req: 12, Files: c}})
		if len(b) >= maxLine {
			t.Fatalf("lot de %d octets", len(b))
		}
		n += len(c)
	}
	if n != len(files) || len(chunks) < 2 {
		t.Fatalf("%d fichiers en %d lots", n, len(chunks))
	}
	var rels []string
	for i := 0; i < 5000; i++ {
		rels = append(rels, strings.Repeat("x", 100))
	}
	n = 0
	for _, c := range ChunkStrings(rels) {
		b, _ := json.Marshal(Msg{Type: TypeRestorePick, Restore: &RestoreData{Rels: c}})
		if len(b) >= maxLine {
			t.Fatalf("lot de %d octets", len(b))
		}
		n += len(c)
	}
	if n != len(rels) || len(ChunkFiles(nil)) != 0 {
		t.Fatal(n)
	}
}
