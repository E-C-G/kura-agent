//go:build windows

package config

import (
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const profileListKey = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\`

// ProfileDir lit ProfileImagePath du compte dans le registre (ProfileList).
// Le profil doit exister, c'est-à-dire que le compte s'est connecté au moins une fois.
func (systemEnv) ProfileDir(account string) (string, error) {
	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return "", fmt.Errorf("compte inconnu : %w", err)
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, profileListKey+sid.String(), registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("aucun profil sur cette machine (le compte ne s'est jamais connecté ?) : %w", err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue("ProfileImagePath")
	if err != nil {
		return "", fmt.Errorf("ProfileImagePath illisible : %w", err)
	}
	return registry.ExpandString(v)
}

func (systemEnv) AccountExists(account string) bool {
	_, _, _, err := windows.LookupSID("", account)
	return err == nil
}
