package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/E-C-G/kura-agent/internal/awscli"
	"github.com/E-C-G/kura-agent/internal/creds"
	"github.com/E-C-G/kura-agent/internal/paths"
	"github.com/E-C-G/kura-agent/internal/setup"
	"github.com/E-C-G/kura-agent/pkg/config"
	"github.com/E-C-G/kura-agent/pkg/pack"
)

// importer regroupe ce que l'import touche, remplaçable dans les tests.
type importer struct {
	out        io.Writer
	now        func() time.Time
	checkAdmin func() error
	ask        func(prompt string) (string, error)
	store      creds.Store
	loadConfig func(path string) (*config.Config, error)
	// testKey vérifie qu'une paire de clés ouvre le bucket de la configuration.
	testKey func(cfg *config.Config, k creds.AWSKeys) error
	remove  func(path string) error
}

func systemImporter() importer {
	return importer{
		out:        os.Stdout,
		now:        time.Now,
		checkAdmin: setup.CheckAdmin,
		ask:        askSecret,
		store:      vault(),
		loadConfig: func(p string) (*config.Config, error) { return config.Load(p, config.LoadOptions{}) },
		testKey:    testAWSKey,
		remove:     os.Remove,
	}
}

func cmdImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	cfgPath := fs.String("config", paths.ConfigFile(), "configuration installée (bucket et aws.exe pour le test de la clé)")
	noTest := fs.Bool("no-test", false, "ne pas tester la clé auprès de Wasabi avant de l'enregistrer")
	force := fs.Bool("force", false, "importer même si le bucket du paquet diffère de celui de la configuration")
	keep := fs.Bool("keep", false, "garder le fichier .kura après l'import")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage : kura-agent import FICHIER.kura [--config FICHIER] [--no-test] [--force] [--keep]")
		return 2
	}
	if err := systemImporter().run(fs.Arg(0), *cfgPath, *noTest, *force, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "ÉCHEC :", err)
		return 2
	}
	return 0
}

func (im importer) run(file, cfgPath string, noTest, force, keep bool) error {
	if err := im.checkAdmin(); err != nil && !errors.Is(err, setup.ErrUnsupported) {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var c pack.Contents
	for try := 1; ; try++ {
		pw, err := im.ask("mot de passe du paquet (affiché par la console) : ")
		if err != nil {
			return fmt.Errorf("lecture du mot de passe : %w", err)
		}
		c, err = pack.Open(data, pw)
		if err == nil {
			break
		}
		if !errors.Is(err, pack.ErrWrongPassword) || try == 3 {
			return err
		}
		fmt.Fprintln(im.out, "mot de passe incorrect")
	}
	if c.Kind == pack.KindInstall {
		return fmt.Errorf("paquet d'installation complète : l'installer avec « kura-agent install --package %s »", file)
	}
	if c.Expired(im.now()) {
		return fmt.Errorf("paquet expiré depuis le %s : en générer un nouveau dans la console", c.ExpiresAt.Local().Format("02/01/2006 15:04"))
	}
	fmt.Fprintf(im.out, "Paquet du %s pour %s / %s, bucket %s\n", c.CreatedAt.Local().Format("02/01/2006 15:04"), c.Client, c.Host, c.Bucket)

	cfg, cfgErr := im.loadConfig(cfgPath)
	if cfg != nil && cfg.Profile.Bucket != c.Bucket && !force {
		return fmt.Errorf("ce paquet est pour le bucket %s, la configuration installée sauvegarde dans %s (--force pour importer quand même)",
			c.Bucket, cfg.Profile.Bucket)
	}
	for _, cr := range c.Credentials {
		if cr.Kind != "aws" {
			continue
		}
		label := fmt.Sprintf("clé %s (%s)", cr.Name, maskKeyID(cr.AccessKeyID))
		// Un autre nom : la clé serait rangée sans servir, la sauvegarde continuerait
		// avec l'ancienne, puis la confirmation dans la console la désactiverait.
		if cfg != nil && cfg.Profile.AWSCredentials != "credential:"+cr.Name && !force {
			return fmt.Errorf("la configuration utilise profile.aws_credentials: %s, le paquet apporte credential:%s : "+
				"la nouvelle clé ne servirait pas. Corriger la configuration, ou --force pour l'enregistrer quand même",
				orNone(cfg.Profile.AWSCredentials), cr.Name)
		}
		if noTest {
			continue
		}
		if cfg == nil {
			return fmt.Errorf("test de la %s impossible sans configuration (%v) : --no-test pour importer sans tester", label, cfgErr)
		}
		// La clé est essayée avant d'être rangée : une clé qui ne fonctionne pas ne
		// remplace jamais celle qui sauvegarde aujourd'hui.
		if err := im.testKey(cfg, creds.AWSKeys{AccessKeyID: cr.AccessKeyID, SecretAccessKey: cr.SecretAccessKey}); err != nil {
			return fmt.Errorf("%s refusée par Wasabi, rien n'a été modifié : %w", label, err)
		}
		fmt.Fprintf(im.out, "OK     %s : accès au bucket %s\n", label, cfg.Profile.Bucket)
	}
	if err := im.store.Init(); err != nil {
		return err
	}
	for _, cr := range c.Credentials {
		value := cr.Value
		if cr.Kind == "aws" {
			if value, err = creds.JoinAWSKeys(cr.AccessKeyID, cr.SecretAccessKey); err != nil {
				return err
			}
		}
		if err := im.store.Set(cr.Name, value); err != nil {
			return fmt.Errorf("enregistrement de %s : %w", cr.Name, err)
		}
		fmt.Fprintf(im.out, "OK     secret %s enregistré\n", cr.Name)
	}
	if !keep {
		if err := im.remove(file); err == nil {
			fmt.Fprintf(im.out, "fichier %s supprimé\n", file)
		}
	}
	fmt.Fprintln(im.out, "Terminé. La prochaine sauvegarde utilisera la nouvelle clé ; confirmer l'installation dans la console une fois cette sauvegarde réussie.")
	return nil
}

// testAWSKey : head-bucket avec la clé donnée, sur le bucket et l'endpoint de la
// configuration.
func testAWSKey(cfg *config.Config, k creds.AWSKeys) error {
	aws, err := resolveAWS(cfg.Profile.AWSCLI)
	if err != nil {
		return err
	}
	cmd := exec.Command(aws, "s3api", "head-bucket", "--bucket", cfg.Profile.Bucket, "--endpoint-url", cfg.Profile.Endpoint)
	cmd.Env = append(append(os.Environ(), awscli.Env(cfg.Profile)...), k.Env()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v : %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func maskKeyID(id string) string {
	if len(id) <= 8 {
		return "…"
	}
	return id[:4] + "…" + id[len(id)-4:]
}

func orNone(s string) string {
	if s == "" {
		return "(aucune)"
	}
	return s
}
