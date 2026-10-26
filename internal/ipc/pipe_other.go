//go:build !windows

package ipc

import (
	"errors"
	"io"
)

var errUnsupported = errors.New("canal de l'icône : Windows uniquement")

// Listener n'existe que sous Windows.
type Listener struct{ Name string }

// Close n'existe que sous Windows.
func (*Listener) Close() error { return errUnsupported }

// Listen n'existe que sous Windows.
func Listen() (*Listener, error) { return nil, errUnsupported }

// Accept n'existe que sous Windows.
func (*Listener) Accept() (io.ReadWriteCloser, error) { return nil, errUnsupported }

// Dial n'existe que sous Windows.
func Dial(int) (io.ReadWriteCloser, error) { return nil, errUnsupported }

// Sessions n'existe que sous Windows.
func Sessions() ([]int, error) { return nil, errUnsupported }

// SessionUser n'existe que sous Windows.
func SessionUser(int) (string, error) { return "", errUnsupported }
