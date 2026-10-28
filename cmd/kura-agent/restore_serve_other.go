//go:build !windows

package main

import (
	"errors"
	"io"
	"os"
	"time"
)

// realServeDeps : la restauration depuis l'icône n'existe que sous Windows (pas
// d'icône ailleurs).
func realServeDeps() (serveDeps, func()) {
	return serveDeps{
		restoreDeps: realRestoreDeps(),
		sessions:    func() ([]int, error) { return nil, errors.New("restauration depuis l'icône : Windows seulement") },
		dial: func(int) (io.ReadWriteCloser, trayUser, error) {
			return nil, nil, errors.New("restauration depuis l'icône : Windows seulement")
		},
		poll: time.Second, idle: time.Minute, linger: 0, logw: os.Stderr,
	}, func() {}
}
