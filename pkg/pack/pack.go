// Package pack définit le paquet .kura : ce que la console kura-ctl remet à un poste
// (clé d'accès seule, ou installation complète : configuration et secrets), chiffré
// par un mot de passe que la console affiche une seule fois.
//
// Format (JSON) :
//
//	{"format":"kura-pack","v":1,"kdf":"pbkdf2-sha256","iter":600000,
//	 "salt":"…","nonce":"…","data":"…"}
//
// data = AES-256-GCM(clé = PBKDF2-SHA256(mot de passe normalisé, salt, iter),
// contenu JSON), avec pour données authentifiées l'en-tête (format, v, kdf, iter,
// salt) : un en-tête modifié rend le paquet illisible.
//
// Ce paquet est partagé par l'agent (qui l'ouvre) et la console (qui le scelle) : un
// seul code fait foi des deux côtés.
package pack

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	formatName = "kura-pack"
	version    = 1
	kdfName    = "pbkdf2-sha256"
	// Iterations : recommandation OWASP pour PBKDF2-HMAC-SHA256.
	Iterations    = 600_000
	maxIterations = 10_000_000
	// MaxSize : taille maximale d'un paquet lu.
	MaxSize = 1 << 20
)

// Types de paquet.
const (
	KindCredentials = "credentials"  // clé seule, configuration inchangée : kura-agent import
	KindInstall     = "installation" // configuration et secrets : kura-agent install --package
)

// MaxConfigSize : taille maximale de la configuration d'un paquet d'installation.
const MaxConfigSize = 256 << 10

// Kinds connus de cette version.
var knownKinds = map[string]bool{KindCredentials: true, KindInstall: true}

// Credential est un secret à ranger dans le coffre du poste sous Name, référencé
// dans la configuration par credential:<Name>.
type Credential struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"` // "aws" (paire de clés) | "secret"
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	Value           string `json:"value,omitempty"`
}

// Contents est le contenu déchiffré d'un paquet.
type Contents struct {
	Kind        string       `json:"kind"`
	CreatedAt   time.Time    `json:"created_at"`
	ExpiresAt   time.Time    `json:"expires_at"`
	Client      string       `json:"client"`
	Host        string       `json:"host"` // poste
	Bucket      string       `json:"bucket"`
	Endpoint    string       `json:"endpoint,omitempty"`
	Region      string       `json:"region,omitempty"`
	Credentials []Credential `json:"credentials"`
	// Config : texte YAML de la configuration du poste (KindInstall). Les secrets n'y
	// figurent que par référence credential:<nom>, leurs valeurs sont dans Credentials.
	Config string `json:"config,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Expired indique si le paquet a dépassé sa date limite.
func (c Contents) Expired(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && now.After(c.ExpiresAt)
}

// Validate vérifie la cohérence du contenu.
// CredentialName : noms de secret acceptés, les mêmes que ceux du coffre de l'agent
// (internal/creds). La console les vérifie avant de créer une clé : un nom refusé à
// l'import laisserait chez Wasabi une clé active sans usage.
var CredentialName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

func (c Contents) Validate() error {
	if !knownKinds[c.Kind] {
		return fmt.Errorf("type de paquet %q inconnu de cette version : mettre à jour kura-agent", c.Kind)
	}
	if c.Bucket == "" {
		return errors.New("paquet sans bucket")
	}
	if len(c.Credentials) == 0 {
		return errors.New("paquet sans clé")
	}
	switch {
	case c.Kind == KindInstall && strings.TrimSpace(c.Config) == "":
		return errors.New("paquet d'installation sans configuration")
	case c.Kind == KindInstall && len(c.Config) > MaxConfigSize:
		return errors.New("configuration du paquet trop grande")
	case c.Kind != KindInstall && c.Config != "":
		return fmt.Errorf("paquet %q avec une configuration", c.Kind)
	}
	seen := map[string]bool{}
	for _, cr := range c.Credentials {
		if !CredentialName.MatchString(cr.Name) {
			return fmt.Errorf("nom de secret invalide : %q (minuscules, chiffres et '-', 40 caractères au plus)", cr.Name)
		}
		if seen[cr.Name] {
			return fmt.Errorf("secret %q en double", cr.Name)
		}
		seen[cr.Name] = true
		switch cr.Kind {
		case "aws":
			if cr.AccessKeyID == "" || cr.SecretAccessKey == "" {
				return fmt.Errorf("secret %q : paire de clés incomplète", cr.Name)
			}
		case "secret":
			if cr.Value == "" {
				return fmt.Errorf("secret %q vide", cr.Name)
			}
		default:
			return fmt.Errorf("secret %q : type %q inconnu", cr.Name, cr.Kind)
		}
	}
	return nil
}

type header struct {
	Format string `json:"format"`
	V      int    `json:"v"`
	KDF    string `json:"kdf"`
	Iter   int    `json:"iter"`
	Salt   []byte `json:"salt"`
}

type envelope struct {
	header
	Nonce []byte `json:"nonce"`
	Data  []byte `json:"data"`
}

// Erreurs d'ouverture.
var (
	ErrFormat        = errors.New("ce fichier n'est pas un paquet kura (.kura)")
	ErrWrongPassword = errors.New("mot de passe incorrect (ou paquet altéré)")
)

// Seal chiffre un paquet. iterations = 0 : valeur par défaut.
func Seal(c Contents, password string, iterations int) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if iterations <= 0 {
		iterations = Iterations
	}
	pw := NormalizePassword(password)
	if len(pw) < 12 {
		return nil, errors.New("mot de passe du paquet trop court")
	}
	h := header{Format: formatName, V: version, KDF: kdfName, Iter: iterations, Salt: make([]byte, 16)}
	if _, err := rand.Read(h.Salt); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(pw, h)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	aad, _ := json.Marshal(h)
	env := envelope{header: h, Nonce: nonce, Data: aead.Seal(nil, nonce, plain, aad)}
	return json.MarshalIndent(env, "", "  ")
}

// Open déchiffre un paquet et vérifie son contenu (pas sa date limite : voir Expired).
func Open(data []byte, password string) (Contents, error) {
	if len(data) > MaxSize {
		return Contents{}, ErrFormat
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil || env.Format != formatName {
		return Contents{}, ErrFormat
	}
	if env.V != version || env.KDF != kdfName {
		return Contents{}, fmt.Errorf("paquet de version %d (%s) inconnue de cette version : mettre à jour kura-agent", env.V, env.KDF)
	}
	if env.Iter < 1 || env.Iter > maxIterations || len(env.Salt) < 16 {
		return Contents{}, ErrFormat
	}
	aead, err := newAEAD(NormalizePassword(password), env.header)
	if err != nil {
		return Contents{}, err
	}
	if len(env.Nonce) != aead.NonceSize() {
		return Contents{}, ErrFormat
	}
	aad, _ := json.Marshal(env.header)
	plain, err := aead.Open(nil, env.Nonce, env.Data, aad)
	if err != nil {
		return Contents{}, ErrWrongPassword
	}
	var c Contents
	if err := json.Unmarshal(plain, &c); err != nil {
		return Contents{}, ErrFormat
	}
	return c, c.Validate()
}

func newAEAD(password string, h header) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, password, h.Salt, h.Iter, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// alphabet : 31 caractères sans 0/O ni 1/I/L, faciles à dicter et à recopier.
const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// NewPassword tire un mot de passe de 5 groupes de 4 caractères (environ 99 bits),
// par exemple « K7QM-2XRT-9HWB-P4NC-VJ3F ».
func NewPassword() (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for i := 0; i < 20; {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		// Rejet des valeurs hors du plus grand multiple de len(alphabet) : tirage uniforme.
		limit := 256 - 256%len(alphabet)
		if int(buf[0]) >= limit {
			continue
		}
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(alphabet[int(buf[0])%len(alphabet)])
		i++
	}
	return b.String(), nil
}

// NormalizePassword ignore la casse, les tirets et les espaces : le mot de passe peut
// être retapé sans les séparateurs.
func NormalizePassword(p string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '\t':
			return -1
		}
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, p)
}
