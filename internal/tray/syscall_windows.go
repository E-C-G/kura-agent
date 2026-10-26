//go:build windows

package tray

import "syscall"

func syscallN(proc uintptr, args ...uintptr) (uintptr, uintptr, syscall.Errno) {
	return syscall.SyscallN(proc, args...)
}
