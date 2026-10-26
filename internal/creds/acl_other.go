//go:build !windows

package creds

import "os"

func secureDir(dir string) error { return os.Chmod(dir, 0o700) }
