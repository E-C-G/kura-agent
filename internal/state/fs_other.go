//go:build !windows

package state

import "os"

func rename(src, dst string) error { return os.Rename(src, dst) }

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
