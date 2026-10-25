package config

import (
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

// ExpandString remplace ${PROFILE:compte} et ${VAR} dans s.
//
// ${PROFILE:compte} est la forme à utiliser pour tout chemin d'utilisateur :
// la tâche tourne sous svc-kura, donc ${LOCALAPPDATA} ou ${USERNAME}
// désigneraient le profil de svc-kura et non celui de l'utilisateur.
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
				firstErr = fmt.Errorf("%s : profil introuvable pour le compte %q : %v", m, arg, err)
				return m
			}
			return toSlash(dir)
		}
		if hasArg {
			firstErr = fmt.Errorf("%s : seule ${PROFILE:compte} accepte un argument", m)
			return m
		}
		v, ok := env.LookupEnv(name)
		if !ok {
			firstErr = fmt.Errorf("%s : variable d'environnement absente", m)
			return m
		}
		return v
	})
	return out, firstErr
}

// Expand développe les variables des champs qui en acceptent.
func (c *Config) Expand(env Env) error {
	var errs ValidationErrors
	exp := func(field string, p *string) {
		v, err := ExpandString(*p, env)
		if err != nil {
			errs = append(errs, FieldError{Field: field, Msg: err.Error()})
			return
		}
		*p = v
	}
	exp("profile.aws_cli", &c.Profile.AWSCLI)
	for i := range c.Jobs {
		j := &c.Jobs[i]
		f := fmt.Sprintf("jobs[%d]", i)
		exp(f+".name", &j.Name)
		exp(f+".source", &j.Source)
		exp(f+".destination", &j.Destination)
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

func toSlash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }
