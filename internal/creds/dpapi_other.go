//go:build !windows

package creds

// Hors Windows : remplaçant de développement, aucun chiffrement (fichier en 0600).
// Les commandes refusent le coffre sur ces systèmes sauf KURA_DEV=1
// (platform.Vault) ; le coffre de production reste à écrire.
func protect(plain []byte) ([]byte, error) { return append([]byte(nil), plain...), nil }

func unprotect(enc []byte) ([]byte, error) { return append([]byte(nil), enc...), nil }
