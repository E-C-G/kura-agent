// Package lock empêche deux exécutions simultanées de run sur la même machine.
package lock

import "errors"

// ErrBusy signale qu'une exécution est déjà en cours.
var ErrBusy = errors.New("une exécution est déjà en cours")

// Name est le nom du verrou machine (mutex Windows visible de toutes les sessions).
const Name = `Global\Kura-Run`

// Release libère le verrou.
type Release func()

// Acquire prend le verrou ou renvoie ErrBusy.
func Acquire() (Release, error) { return acquire(Name) }
