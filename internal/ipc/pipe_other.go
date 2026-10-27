//go:build !windows

package ipc

import (
	"io"

	"github.com/E-C-G/kura-agent/internal/platform"
)

var errUnsupported = platform.NotImplemented(platform.Tray)

// Listener : canal pas encore écrit pour ce système.
type Listener struct{ Name string }

// Close n'est pas encore écrit pour ce système.
func (*Listener) Close() error { return errUnsupported }

// Listen n'est pas encore écrit pour ce système.
func Listen() (*Listener, error) { return nil, errUnsupported }

// Accept n'est pas encore écrit pour ce système.
func (*Listener) Accept() (io.ReadWriteCloser, error) { return nil, errUnsupported }

// Dial n'est pas encore écrit pour ce système.
func Dial(int) (io.ReadWriteCloser, error) { return nil, errUnsupported }

// Sessions n'est pas encore écrit pour ce système.
func Sessions() ([]int, error) { return nil, errUnsupported }

// SessionUser n'est pas encore écrit pour ce système.
func SessionUser(int) (string, error) { return "", errUnsupported }
