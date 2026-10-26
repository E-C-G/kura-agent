//go:build !windows

package awsinstall

func signature(string) (string, string, error) { return "", "", ErrUnsupported }

func runMSI(string, string) (bool, error) { return false, ErrUnsupported }
