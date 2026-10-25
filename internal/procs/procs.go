// Package procs trouve et arrête des processus par nom, toutes sessions confondues.
//
// Sur un Windows Server multi-sessions, run tourne sous svc-kura (session 0) alors
// qu'Outlook tourne dans la session de l'utilisateur : seul un arrêt forcé est
// possible d'ici. La fermeture douce passera par l'icône de la session (étape 5).
package procs

// Process est un processus en cours.
type Process struct {
	PID     int
	Name    string
	Session int // numéro de session Windows (0 hors Windows)
}

// Manager liste et arrête des processus.
type Manager interface {
	Find(name string) ([]Process, error)
	Terminate(p Process) error
}

// System renvoie le Manager de la machine courante.
func System() Manager { return system{} }

type system struct{}
