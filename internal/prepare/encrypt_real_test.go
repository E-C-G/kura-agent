package prepare

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/E-C-G/kura-agent/internal/state"
	"github.com/E-C-G/kura-agent/pkg/config"
)

// Essais avec les vrais outils, lancés par la CI : chiffré par l'agent, déchiffré par
// l'outil age officiel (KURA_TEST_AGE) ou par GnuPG (KURA_TEST_GPG) avec la clé privée
// de test, qui ne passe jamais par l'agent.

func plainTree(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "Contrats")
	for name, data := range map[string]string{"bail.pdf": "bail signé", "sous/avenant.txt": "avenant n°2"} {
		p := filepath.Join(src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(data), 0o644)
	}
	return src
}

func TestAgeRealDecrypt(t *testing.T) {
	ageTool := needEnv(t, "KURA_TEST_AGE")[0]
	id := newAgeIdentity(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keys := t.TempDir()
	os.WriteFile(filepath.Join(keys, "id.txt"), []byte(id.String()+"\n"), 0o600)
	os.WriteFile(filepath.Join(keys, "id_ed25519"), pem.EncodeToMemory(block), 0o600)

	cfg := &config.Config{Prepare: []config.Prepare{{ID: "contrats", Type: config.PrepareAge, Source: plainTree(t),
		Recipients: []config.Recipient{{Age: id.Recipient().String()}, {Age: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))}}}}}
	ps, dir, log := realRun(t, cfg, "")
	if ps.Result != state.ResultOK || ps.Files != 2 {
		t.Fatalf("%+v\n%s", ps, log)
	}
	// Chacune des deux clés déchiffre, avec l'outil officiel.
	for _, idFile := range []string{"id.txt", "id_ed25519"} {
		out := admin(t, nil, nil, ageTool, "--decrypt", "-i", filepath.Join(keys, idFile), filepath.Join(dir, "sous", "avenant.txt.age"))
		if out != "avenant n°2" {
			t.Fatalf("%s : %q", idFile, out)
		}
	}
}

// gpgKey crée une clé de test (principale et sous-clé de chiffrement) dans son propre
// trousseau ; renvoie l'empreinte et la clé publique ASCII.
func testGPGKey(t *testing.T, gpg, home, uid string) (string, string) {
	t.Helper()
	base := []string{"--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", ""}
	admin(t, nil, nil, gpg, append(base, "--quick-gen-key", uid, "default", "default", "never")...)
	colons := admin(t, nil, nil, gpg, append(base, "--with-colons", "--list-keys", uid)...)
	fpr := ""
	for _, l := range strings.Split(colons, "\n") {
		if f := strings.Split(l, ":"); f[0] == "fpr" && fpr == "" {
			fpr = f[9]
		}
	}
	key := admin(t, nil, nil, gpg, append(base, "--armor", "--export", fpr)...)
	return fpr, key
}

func TestGPGRealEncrypt(t *testing.T) {
	gpg := needEnv(t, "KURA_TEST_GPG")[0]
	home := filepath.Join(t.TempDir(), "gnupg")
	os.Mkdir(home, 0o700)
	t.Cleanup(func() {
		if conf := sibling(gpg, "gpgconf"); conf != "" {
			admin(t, nil, nil, conf, "--homedir", home, "--kill", "all")
		}
	})
	fpr1, key1 := testGPGKey(t, gpg, home, "Kura Éditeur <editeur@kura.invalid>")
	fpr2, key2 := testGPGKey(t, gpg, home, "Kura Client <client@kura.invalid>")
	cfg := &config.Config{Tools: config.Tools{GPG: gpg}, Prepare: []config.Prepare{{ID: "contrats", Type: config.PrepareGPG,
		Source: plainTree(t), Recipients: []config.Recipient{{Fingerprint: fpr1, Key: key1}, {Fingerprint: fpr2, Key: key2}}}}}
	ps, dir, log := realRun(t, cfg, "")
	if ps.Result != state.ResultOK || ps.Files != 2 || len(ps.Recipients) != 2 {
		t.Fatalf("%+v\n%s", ps, log)
	}
	out := admin(t, nil, nil, gpg, "--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", "",
		"--decrypt", filepath.Join(dir, "bail.pdf.gpg"))
	if out != "bail signé" {
		t.Fatalf("déchiffré : %q", out)
	}
	// Empreinte qui ne correspond pas à la clé fournie : refusé, rien de chiffré.
	cfg.Prepare[0].Recipients = []config.Recipient{{Fingerprint: fpr2, Key: key1}}
	if ps, _, _ = realRun(t, cfg, ""); ps.Result != state.ResultError || !strings.Contains(ps.Message, "sans être déclarée") &&
		!strings.Contains(ps.Message, "empreinte absente") {
		t.Fatalf("empreinte fausse acceptée : %+v", ps)
	}
}
