//go:build !windows

package setup

// CheckAdmin : installation réservée à Windows.
func CheckAdmin() error { return ErrUnsupported }

// LocalAccountSID : Windows uniquement.
func LocalAccountSID(string) (string, error) { return "", ErrUnsupported }

// AccountSID : Windows uniquement.
func AccountSID(string) (string, error) { return "", ErrUnsupported }

// EnsureAccount : Windows uniquement.
func EnsureAccount(string) (string, bool, error) { return "", false, ErrUnsupported }

// RemoveAccount : Windows uniquement.
func RemoveAccount(string) error { return ErrUnsupported }

// SecureDataDir : Windows uniquement.
func SecureDataDir(string) error { return ErrUnsupported }

// ProgramDir : Windows uniquement.
func ProgramDir() string { return "" }

// InstallBinary : Windows uniquement.
func InstallBinary(string) (string, error) { return "", ErrUnsupported }

// RemoveProgramDir : Windows uniquement.
func RemoveProgramDir(string) (bool, error) { return false, ErrUnsupported }
