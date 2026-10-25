//go:build !windows

package creds

// Hors Windows (développement uniquement) : aucun chiffrement, fichier en 0600.
func protect(plain []byte) ([]byte, error) { return append([]byte(nil), plain...), nil }

func unprotect(enc []byte) ([]byte, error) { return append([]byte(nil), enc...), nil }
