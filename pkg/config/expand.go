package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var errEOF = io.EOF

// Env fournit à la configuration ce qu'elle doit savoir de la machine.
type Env interface {
	// LookupEnv lit une variable d'environnement du compte qui exécute l'agent.
	LookupEnv(name string) (string, bool)
	// ProfileDir renvoie le dossier de profil d'un compte (ex. C:\Users\alice).
	ProfileDir(account string) (string, error)
	// AccountExists indique si le compte existe sur la machine.
	AccountExists(account string) bool
}

// SystemEnv renvoie l'Env de la machine courante.
func SystemEnv() Env { return systemEnv{} }

type systemEnv struct{}

func (systemEnv) LookupEnv(name string) (string, bool) { return os.LookupEnv(name) }

// ${NOM} ou ${PROFILE:compte}
var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::([^}]*))?\}`)

// UnresolvedError : la variable est bien formée mais ne se résout pas sur cette machine
// (compte sans profil, variable absente). Pour la source d'un job, l'erreur ne touche
// que ce job (Job.Unavailable) : un compte supprimé ne doit pas arrêter la sauvegarde
// des autres dossiers.
type UnresolvedError struct{ msg string }

func (e UnresolvedError) Error() string { return e.msg }

// ExpandString remplace ${PROFILE:compte} et ${VAR} dans s.
//
// ${PROFILE:compte} est la forme à utiliser pour tout chemin d'utilisateur :
// la tâche tourne sous SYSTEM, donc ${LOCALAPPDATA} ou ${USERNAME}
// désigneraient le profil de SYSTEM et non celui de l'utilisateur.
func ExpandString(s string, env Env) (string, error) {
	var firstErr error
	out := varPattern.ReplaceAllStringFunc(s, func(m string) string {
		if firstErr != nil {
			return m
		}
		sub := varPattern.FindStringSubmatch(m)
		name, arg := sub[1], sub[2]
		hasArg := strings.Contains(m, ":")
		if name == "PROFILE" {
			if !hasArg || arg == "" {
				firstErr = fmt.Errorf("%s : précisez le compte, ex. ${PROFILE:alice}", m)
				return m
			}
			dir, err := env.ProfileDir(arg)
			if err != nil {
				firstErr = UnresolvedError{fmt.Sprintf("%s : profil introuvable pour le compte %q : %v", m, arg, err)}
				return m
			}
			return toSlash(dir)
		}
		if name == "STAGING" {
			firstErr = fmt.Errorf("%s : ${STAGING:<id>} s'emploie seul, comme source d'un job", m)
			return m
		}
		if hasArg {
			firstErr = fmt.Errorf("%s : seule ${PROFILE:compte} accepte un argument", m)
			return m
		}
		v, ok := env.LookupEnv(name)
		if !ok {
			firstErr = UnresolvedError{fmt.Sprintf("%s : variable d'environnement absente", m)}
			return m
		}
		return v
	})
	return out, firstErr
}

// Expand développe les variables des champs qui en acceptent.
func (c *Config) Expand(env Env) error {
	var errs ValidationErrors
	exp := func(field string, p *string, job *Job) {
		v, err := ExpandString(*p, env)
		var unresolved UnresolvedError
		switch {
		case err == nil:
			*p = v
		case job != nil && errors.As(err, &unresolved):
			if job.Unavailable == "" {
				job.Unavailable = field + " : " + err.Error()
			}
		default:
			errs = append(errs, FieldError{Field: field, Msg: err.Error()})
		}
	}
	exp("profile.aws_cli", &c.Profile.AWSCLI, nil)
	for i := range c.Prepare {
		p := &c.Prepare[i]
		if p.Source == "" {
			continue
		}
		v, err := ExpandString(p.Source, env)
		var unresolved UnresolvedError
		switch {
		case err == nil:
			p.Source = v
		case errors.As(err, &unresolved):
			p.Unavailable = fmt.Sprintf("prepare[%d].source : %v", i, err)
		default:
			errs = append(errs, FieldError{Field: fmt.Sprintf("prepare[%d].source", i), Msg: err.Error()})
		}
	}
	for i := range c.Jobs {
		j := &c.Jobs[i]
		f := fmt.Sprintf("jobs[%d]", i)
		exp(f+".name", &j.Name, j)
		if m := stagingSource.FindStringSubmatch(j.Source); m != nil {
			j.Prepare = m[1] // résolue par ResolveStaging, sur la machine de l'agent
		} else {
			exp(f+".source", &j.Source, j)
		}
		exp(f+".destination", &j.Destination, j)
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// stagingSource : source d'un job tirée d'une préparation, ${STAGING:<id>}.
var stagingSource = regexp.MustCompile(`^\$\{STAGING:([a-z0-9-]+)\}$`)

// ResolveStaging remplace la source ${STAGING:<id>} des jobs par <dir>/<id>, le dossier
// de la préparation dans la zone de préparation de cette machine.
func (c *Config) ResolveStaging(dir string) {
	for i := range c.Jobs {
		if j := &c.Jobs[i]; j.Prepare != "" {
			j.Source = toSlash(dir) + "/" + j.Prepare
		}
	}
}

func toSlash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }
