// Package procs trouve et arrête des processus par nom, toutes sessions confondues.
//
// Sur un Windows Server multi-sessions, run tourne sous SYSTEM (session 0) alors
// qu'Outlook tourne dans la session de l'utilisateur : seul un arrêt forcé est
// possible d'ici. La fermeture douce passe par l'icône de la session.
package procs

// Process est un processus en cours.
type Process struct {
	PID     int
	Name    string
	Session int    // numéro de session Windows (0 hors Windows)
	Path    string // chemin de l'exécutable, s'il est lisible (relance par l'icône)
}

// Manager liste et arrête des processus.
type Manager interface {
	Find(name string) ([]Process, error)
	Terminate(p Process) error
}

// System renvoie le Manager de la machine courante.
func System() Manager { return system{} }

type system struct{}
