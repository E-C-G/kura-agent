module github.com/E-C-G/kura-agent

go 1.26.0

// Compilateur des binaires publiés (actions/setup-go lit cette ligne) : dernier
// correctif de Go 1.26, à suivre à chaque publication de sécurité.
toolchain go1.26.10

require (
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/sys v0.49.0
)
