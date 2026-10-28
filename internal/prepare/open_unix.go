//go:build !windows

package prepare

import "syscall"

// openNoBlock : un fichier remplacé par un tube nommé entre le parcours et l'ouverture ne
// bloque pas la copie (il est ensuite écarté : pas un fichier ordinaire).
const openNoBlock = syscall.O_NONBLOCK
