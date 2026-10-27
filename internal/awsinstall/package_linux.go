package awsinstall

import "runtime"

// SystemPackage : zip officiel pour l'architecture de la machine et sa signature PGP,
// vérifiée avec la clé « AWS CLI Team » intégrée.
func SystemPackage() Package { return linuxPackage(runtime.GOARCH) }

// DefaultPath : lien créé par aws/install.
func DefaultPath() string { return linuxBinDir + "/aws" }
