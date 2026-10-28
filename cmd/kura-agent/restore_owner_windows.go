package main

import "github.com/E-C-G/kura-agent/internal/restore"

// destOwner : sous Windows, les fichiers héritent des droits du dossier qui les reçoit.
func destOwner(string) (restore.Owner, error) { return restore.Owner{}, nil }
