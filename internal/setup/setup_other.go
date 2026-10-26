//go:build !windows

package setup

// CheckAdmin : installation réservée à Windows.
func CheckAdmin() error { return ErrUnsupported }

// LocalAccountSID : Windows uniquement.
func LocalAccountSID(string) (string, error) { return "", ErrUnsupported }

// AccountSID : Windows uniquement.
func AccountSID(string) (string, error) { return "", ErrUnsupported }

// RemoveAccount : Windows uniquement.
func RemoveAccount(string) error { return ErrUnsupported }

// SecureDataDir : Windows uniquement.
func SecureDataDir(string) error { return ErrUnsupported }

// ProgramDir : Windows uniquement.
func ProgramDir() string { return "" }

// GUIName est la version sans console de l'exécutable.
const GUIName = "kura-agentw.exe"

// InstallBinary : Windows uniquement.
func InstallBinary(string) (string, string, error) { return "", "", ErrUnsupported }

// SetTrayAutostart : Windows uniquement.
func SetTrayAutostart(string) error { return ErrUnsupported }

// RemoveTrayAutostart : Windows uniquement.
func RemoveTrayAutostart() error { return ErrUnsupported }

// StopTrays : Windows uniquement.
func StopTrays() int { return 0 }

// RemoveProgramDir : Windows uniquement.
func RemoveProgramDir(string) (bool, error) { return false, ErrUnsupported }
