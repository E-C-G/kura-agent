//go:build !windows

package prepare

import "errors"

// msGrant : SQL Server n'est exporté que sous Windows.
func msGrant(string, string) (func() error, error) {
	return nil, errors.New("export SQL Server : Windows uniquement")
}
