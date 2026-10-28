package restore

import "io/fs"

// fileOwner : sans objet sous Windows (droits hérités du dossier).
func fileOwner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
