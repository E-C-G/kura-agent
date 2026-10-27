//go:build !windows && !linux

package creds

// System : coffre pas encore écrit pour ce système ; remplaçant en clair, refusé par
// les commandes sauf KURA_DEV=1 (platform.Vault).
func System(dir string) Store { return Store{Dir: dir} }
