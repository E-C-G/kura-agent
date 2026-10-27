package creds

import (
	"bytes"
	"crypto/aes"
	gcm "crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Trousseau macOS : chaque secret est chiffré en AES-256-GCM (le nom du fichier
// authentifié avec lui : un fichier renommé est refusé) par une clé de 256 bits
// rangée dans le trousseau Système, que seul root ouvre. La clé est créée au premier
// secret ; elle ne passe jamais en argument d'une commande (visible de tous avec ps) :
// security la reçoit sur son entrée standard (mode -i).
const (
	// SystemKeychain : trousseau de la machine.
	SystemKeychain = "/Library/Keychains/System.keychain"
	// KeychainService et KeychainAccount désignent la clé du coffre dans le trousseau.
	KeychainService = "com.e-c-g.kura.vault"
	KeychainAccount = "master"

	keychainMagic = "KKC1"
	// notFoundCode : code de sortie de security quand l'élément n'existe pas.
	notFoundCode = 44
)

// securityRunner lance /usr/bin/security avec stdin ; renvoie la sortie standard et
// le code de sortie (0 : succès).
type securityRunner func(stdin string, args ...string) (out string, code int, err error)

// keychainCipher chiffre avec la clé du trousseau path.
type keychainCipher struct {
	path string
	run  securityRunner

	mu  sync.Mutex
	key []byte
}

func newKeychainCipher(path string, run securityRunner) *keychainCipher {
	return &keychainCipher{path: path, run: run}
}

// masterKey relit la clé, et la crée si create et qu'elle manque.
func (k *keychainCipher) masterKey(create bool) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key != nil {
		return k.key, nil
	}
	key, found, err := k.find()
	if err != nil {
		return nil, err
	}
	if !found {
		if !create {
			return nil, fmt.Errorf("clé du coffre absente du trousseau %s", k.path)
		}
		if strings.ContainsAny(k.path, " \t\n\"'") {
			return nil, fmt.Errorf("trousseau %q : chemin avec espace ou guillemet refusé", k.path)
		}
		fresh := make([]byte, 32)
		if _, err := rand.Read(fresh); err != nil {
			return nil, err
		}
		// -A : lisible sans demande d'autorisation (aucune fenêtre ne peut s'ouvrir
		// pour un démon) ; seul root ouvre le trousseau Système.
		cmd := fmt.Sprintf("add-generic-password -s %s -a %s -l %s -A -w %s %s\n",
			KeychainService, KeychainAccount, "Kura", base64.StdEncoding.EncodeToString(fresh), k.path)
		if _, code, err := k.run(cmd, "-i"); err != nil || code != 0 {
			return nil, fmt.Errorf("création de la clé du coffre dans %s : code %d %v", k.path, code, err)
		}
		// security -i ne signale pas toujours un échec : la clé est relue.
		if key, found, err = k.find(); err != nil || !found {
			return nil, fmt.Errorf("clé du coffre introuvable après création dans %s (%v)", k.path, err)
		}
		if !bytes.Equal(key, fresh) {
			return nil, errors.New("clé du coffre relue différente de celle créée")
		}
	}
	k.key = key
	return key, nil
}

func (k *keychainCipher) find() ([]byte, bool, error) {
	out, code, err := k.run("", "find-generic-password", "-s", KeychainService, "-a", KeychainAccount, "-w", k.path)
	if code == notFoundCode {
		return nil, false, nil
	}
	if err != nil || code != 0 {
		return nil, false, fmt.Errorf("lecture de la clé du coffre dans %s : code %d %v (lancer avec sudo)", k.path, code, err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil || len(key) != 32 {
		return nil, false, fmt.Errorf("clé du coffre illisible dans %s", k.path)
	}
	return key, true, nil
}

func (k *keychainCipher) aead(create bool) (gcm.AEAD, error) {
	key, err := k.masterKey(create)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return gcm.NewGCM(block)
}

func (k *keychainCipher) protect(name string, plain []byte) ([]byte, error) {
	g, err := k.aead(true)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(keychainMagic), nonce...)
	return g.Seal(out, nonce, plain, []byte(name)), nil
}

func (k *keychainCipher) unprotect(name string, enc []byte) ([]byte, error) {
	g, err := k.aead(false)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(enc, []byte(keychainMagic)) || len(enc) < len(keychainMagic)+g.NonceSize() {
		return nil, errors.New("format de secret inconnu")
	}
	enc = enc[len(keychainMagic):]
	plain, err := g.Open(nil, enc[:g.NonceSize()], enc[g.NonceSize():], []byte(name))
	if err != nil {
		return nil, errors.New("secret altéré ou chiffré par une autre clé")
	}
	return plain, nil
}

// runSecurity lance /usr/bin/security.
func runSecurity(stdin string, args ...string) (string, int, error) {
	cmd := exec.Command("/usr/bin/security", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return "", -1, err
	}
	return string(out), 0, nil
}

// DeleteSystemKey retire la clé du coffre du trousseau Système (uninstall --purge) ;
// absente n'est pas une erreur.
func DeleteSystemKey() error {
	_, code, err := runSecurity("", "delete-generic-password", "-s", KeychainService, "-a", KeychainAccount, SystemKeychain)
	if code == 0 || code == notFoundCode {
		return nil
	}
	return fmt.Errorf("clé du coffre dans %s : code %d %v", SystemKeychain, code, err)
}
