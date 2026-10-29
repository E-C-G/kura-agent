//go:build windows

package prepare

import (
	"archive/tar"
	"context"
	"errors"
)

// tarTree : docker_volume n'existe que sous Linux.
func tarTree(context.Context, string, func(string, bool) bool, *tar.Writer) (tarStats, error) {
	return tarStats{}, errors.New("docker_volume : Linux seulement")
}
