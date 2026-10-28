package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"time"

	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/selfupdate"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Codes de sortie de self-update.
const (
	exitUpdateOK      = 0 // à jour (ou mis à jour)
	exitUpdateNewer   = 3 // --check : version plus récente disponible
	exitUpdateRefused = 4 // refusé (droits, sauvegarde en cours, version inférieure…)
	exitUpdateFailed  = 5 // recherche ou vérification en échec, rien n'a été modifié
)

type selfUpdateOut struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	Newer     bool      `json:"newer"`
	Verified  bool      `json:"verified"`
	Asset     string    `json:"asset,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	Published time.Time `json:"published,omitzero"`
	NotesURL  string    `json:"notes_url,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func cmdSelfUpdate(args []string) int {
	return runSelfUpdate(args, os.Stdout, os.Stderr, nil)
}

// runSelfUpdate : client injecté par les tests (nil : GitHub).
func runSelfUpdate(args []string, stdout, stderr io.Writer, client *selfupdate.Client) int {
	fs := flag.NewFlagSet("self-update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "cherche une version plus récente et vérifie son attestation, sans rien modifier")
	tag := fs.String("version", "", "version visée (vX.Y.Z) au lieu de la dernière")
	asJSON := fs.Bool("json", false, "résultat en JSON")
	cfgPath := fs.String("config", paths.ConfigFile(), "fichier de configuration (pour update.proxy)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return 2
	}
	if !*check {
		fmt.Fprintln(stderr, "L'installation d'une nouvelle version par self-update arrive avec kura-agent 0.10.0.\n"+
			"Pour l'instant : kura-agent self-update --check (recherche et vérification, sans rien modifier).")
		return exitUpdateRefused
	}
	if client == nil {
		proxy, err := updateProxy(*cfgPath, stderr)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUpdateFailed
		}
		hc, err := selfupdate.NewHTTPClient(proxy)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUpdateFailed
		}
		client = &selfupdate.Client{HTTP: hc, UserAgent: "kura-agent/" + version}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := client.Check(ctx, selfupdate.Options{Current: version, Tag: *tag, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH})
	out := selfUpdateOut{Current: version, Newer: res.Newer, Verified: res.Verified}
	if res.Latest.Tag != "" {
		out.Latest, out.Published, out.NotesURL = res.Latest.Version, res.Latest.Published, res.Latest.NotesURL
	}
	if res.Verified {
		out.Asset, out.SHA256 = res.Asset.Name, res.Asset.SHA256
	}
	code := exitUpdateOK
	switch {
	case errors.Is(err, selfupdate.ErrDevVersion):
		out.Error = err.Error()
	case err != nil:
		out.Error = err.Error()
		code = exitUpdateFailed
	case res.Newer:
		code = exitUpdateNewer
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return code
	}
	fmt.Fprintf(stdout, "Version en place  : %s\n", out.Current)
	if out.Latest != "" {
		fmt.Fprintf(stdout, "Dernière version  : %s (publiée le %s)\n", out.Latest, out.Published.Local().Format("02/01/2006"))
	}
	if out.Verified {
		fmt.Fprintf(stdout, "Archive           : %s\n                    attestation de release GitHub vérifiée (sha256 %s)\n", out.Asset, out.SHA256)
	}
	if out.NotesURL != "" {
		fmt.Fprintf(stdout, "Notes de version  : %s\n", out.NotesURL)
	}
	switch {
	case code == exitUpdateFailed:
		fmt.Fprintf(stderr, "Échec : %s\n", out.Error)
	case out.Error != "":
		fmt.Fprintf(stdout, "%s.\n", out.Error)
	case out.Newer:
		fmt.Fprintln(stdout, "Mise à jour disponible.")
	default:
		fmt.Fprintln(stdout, "À jour.")
	}
	return code
}

// updateProxy lit update.proxy ; fichier absent : pas de proxy configuré.
func updateProxy(path string, stderr io.Writer) (string, error) {
	u, err := config.ReadUpdate(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		fmt.Fprintf(stderr, "Attention : %s illisible (%v) ; update.proxy non appliqué.\n", path, err)
		return "", nil
	}
	return u.Proxy, nil
}
