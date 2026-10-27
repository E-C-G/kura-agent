//go:build !windows

package awsinstall

import "github.com/E-C-G/kura-agent/internal/platform"

func signature(string) (string, string, error) {
	return "", "", platform.NotImplemented(platform.AWSCLISetup)
}

func runMSI(string, string) (bool, error) {
	return false, platform.NotImplemented(platform.AWSCLISetup)
}
