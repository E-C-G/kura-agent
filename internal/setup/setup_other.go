//go:build !windows

package setup

// LocalAccountSID : pas encore écrit pour ce système.
func LocalAccountSID(string) (string, error) { return "", notImplemented() }

// AccountSID : pas encore écrit pour ce système.
func AccountSID(string) (string, error) { return "", notImplemented() }

// RemoveAccount : pas encore écrit pour ce système.
func RemoveAccount(string) error { return notImplemented() }

// SecureDataDir : pas encore écrit pour ce système.
func SecureDataDir(string) error { return notImplemented() }

// ProgramDir : pas encore écrit pour ce système.
func ProgramDir() string { return "" }

// GUIName est la version sans console de l'exécutable.
const GUIName = "kura-agentw.exe"

// InstallBinary : pas encore écrit pour ce système.
func InstallBinary(string) (string, string, error) { return "", "", notImplemented() }

// SetTrayAutostart : pas encore écrit pour ce système.
func SetTrayAutostart(string) error { return notImplemented() }

// RemoveTrayAutostart : pas encore écrit pour ce système.
func RemoveTrayAutostart() error { return notImplemented() }

// StopTrays : pas encore écrit pour ce système.
func StopTrays() int { return 0 }

// RemoveProgramDir : pas encore écrit pour ce système.
func RemoveProgramDir(string) (bool, error) { return false, notImplemented() }
