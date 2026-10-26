//go:build windows

package creds

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Entropie fixe : un autre programme du même compte ne déchiffre pas ces secrets
// par un simple appel DPAPI sans la connaître.
var entropy = []byte("kura-agent/creds/v1")

func blob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func takeBlob(out *windows.DataBlob) []byte {
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...)
}

// protect chiffre dans la portée de la machine : tout compte de ce poste peut déchiffrer
// le blob, c'est l'ACL du dossier (SYSTEM et Administrateurs seuls) qui en réserve la
// lecture. La tâche (svc-kura, sans mot de passe stocké) et une console d'administration
// lisent donc les mêmes secrets. Copié sur une autre machine, le blob est illisible.
// unprotect relit aussi les blobs en portée utilisateur écrits par la v0.2.0.
func protect(plain []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptProtectData(blob(plain), nil, blob(entropy), 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN|windows.CRYPTPROTECT_LOCAL_MACHINE, &out); err != nil {
		return nil, err
	}
	return takeBlob(&out), nil
}

func unprotect(enc []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blob(enc), nil, blob(entropy), 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return takeBlob(&out), nil
}
