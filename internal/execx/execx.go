// Package execx lance les sous-processus (aws.exe) sans fenêtre et en garantit l'arrêt.
package execx

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

const waitDelay = 5 * time.Second

// Executor lance une commande et renvoie son code de sortie.
// Une interface, pour que le moteur se teste avec une fausse CLI AWS.
type Executor interface {
	Run(ctx context.Context, name string, args, env []string, out io.Writer) (exitCode int, err error)
}

// System lance de vrais processus.
type System struct{}

// Run lance name avec args ; stdout et stderr vont dans out ; env s'ajoute à
// l'environnement courant. err n'est renseigné que si le processus n'a pas pu
// démarrer ou a été interrompu par ctx ; un code de sortie non nul n'est pas une erreur.
func (System) Run(ctx context.Context, name string, args, env []string, out io.Writer) (int, error) {
	return run(ctx, Spec{Name: name, Args: args, Env: append(os.Environ(), env...), Out: out})
}

// Spec décrit un lancement entièrement maîtrisé : environnement complet (il remplace
// celui de l'agent au lieu de s'y ajouter), dossier de travail, entrée standard.
type Spec struct {
	Name  string
	Args  []string
	Env   []string  // environnement complet du processus
	Dir   string    // dossier de travail ; vide : celui de l'agent
	Stdin io.Reader // nil : aucune entrée
	Out   io.Writer // stdout, et stderr si ErrOut est nil
	// ErrOut reçoit stderr à part (journal), quand stdout porte le résultat (export
	// d'une base, liste lue par l'agent) ; nil : stderr va dans Out.
	ErrOut io.Writer
}

// RunSpec lance s ; mêmes règles que Run.
func (System) RunSpec(ctx context.Context, s Spec) (int, error) { return run(ctx, s) }

func run(ctx context.Context, s Spec) (int, error) {
	cmd := exec.CommandContext(ctx, s.Name, s.Args...)
	cmd.Env = s.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = s.Dir
	cmd.Stdin = s.Stdin
	cmd.Stdout = s.Out
	cmd.Stderr = s.Out
	if s.ErrOut != nil {
		cmd.Stderr = s.ErrOut
	}
	// Après une annulation, un petit-enfant peut garder la sortie ouverte : on
	// n'attend pas sa fin plus de WaitDelay avant de fermer les tubes.
	cmd.WaitDelay = waitDelay
	platformSetup(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// KillChildrenOnExit garantit que les sous-processus meurent avec l'agent
// (tâche planifiée arrêtée, processus tué). Sans effet hors Windows.
func KillChildrenOnExit() error { return killChildrenOnExit() }
