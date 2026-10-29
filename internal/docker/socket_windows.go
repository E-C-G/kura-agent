//go:build windows

package docker

import "errors"

// CheckSocket : docker_volume n'existe que sous Linux.
func CheckSocket(string) error { return errors.New("docker_volume : Linux seulement") }
