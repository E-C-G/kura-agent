package awsinstall

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AWSCLIKeyFingerprint : empreinte publiée par AWS de la clé qui signe la CLI pour
// Linux (« AWS CLI Team <aws-cli@amazon.com> », valable jusqu'au 1er juillet 2027 :
// une nouvelle clé ou une prolongation demande une mise à jour de kura-agent).
const AWSCLIKeyFingerprint = "FB5DB77FD5C118B80511ADA8A6310ACC4672475C"

// awsCLIKey : clé publique, copiée de la documentation d'installation de la CLI AWS
// (getting-started-install) ; son empreinte est vérifiée par les tests.
const awsCLIKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mQINBF2Cr7UBEADJZHcgusOJl7ENSyumXh85z0TRV0xJorM2B/JL0kHOyigQluUG
ZMLhENaG0bYatdrKP+3H91lvK050pXwnO/R7fB/FSTouki4ciIx5OuLlnJZIxSzx
PqGl0mkxImLNbGWoi6Lto0LYxqHN2iQtzlwTVmq9733zd3XfcXrZ3+LblHAgEt5G
TfNxEKJ8soPLyWmwDH6HWCnjZ/aIQRBTIQ05uVeEoYxSh6wOai7ss/KveoSNBbYz
gbdzoqI2Y8cgH2nbfgp3DSasaLZEdCSsIsK1u05CinE7k2qZ7KgKAUIcT/cR/grk
C6VwsnDU0OUCideXcQ8WeHutqvgZH1JgKDbznoIzeQHJD238GEu+eKhRHcz8/jeG
94zkcgJOz3KbZGYMiTh277Fvj9zzvZsbMBCedV1BTg3TqgvdX4bdkhf5cH+7NtWO
lrFj6UwAsGukBTAOxC0l/dnSmZhJ7Z1KmEWilro/gOrjtOxqRQutlIqG22TaqoPG
fYVN+en3Zwbt97kcgZDwqbuykNt64oZWc4XKCa3mprEGC3IbJTBFqglXmZ7l9ywG
EEUJYOlb2XrSuPWml39beWdKM8kzr1OjnlOm6+lpTRCBfo0wa9F8YZRhHPAkwKkX
XDeOGpWRj4ohOx0d2GWkyV5xyN14p2tQOCdOODmz80yUTgRpPVQUtOEhXQARAQAB
tCFBV1MgQ0xJIFRlYW0gPGF3cy1jbGlAYW1hem9uLmNvbT6JAlQEEwEIAD4CGwMF
CwkIBwIGFQoJCAsCBBYCAwECHgECF4AWIQT7Xbd/1cEYuAURraimMQrMRnJHXAUC
akV0ygUJDqP4lQAKCRCmMQrMRnJHXFHjD/9eyZLYcKuQOlLvtqSDtUBiEZf6ZZjM
i3ygYH8rJNtuToUH+HvSpe819urJCquXhDrlK6N+aqW0hCLtNABJG/vsafIgvIYJ
hSGgpgtNnQyMV1jViRWqPjbouw8OkYKBThUfT1i2Y+wn58ifs6ODBCmTexWtXspA
Si+Gt49xDOW0APmbOPnI+a4HJW6tVEo6MWS0WjzpiBayR3d1A4pt4YrPfSdDgpLo
h2SLQqlRqvvVZJaWBjhkErNFpfsBA06sDcPEOb0G8LBUbR4WOcdvhe5LubJbZuxC
AG9kNPCVeQP1ixwjgjXKysaxeQ6rv0VzIQgRp6tLVLWhy6AKDNvLjFSsmXZ1Wl08
Y/RlOHXlzLuQMRE6sR1wOdRxc9TsrNWTGiBK65cvSWOy03JeBkQQ8pesqltiyxI9
U21kkgiXtTSKNGfKK8pO27D81YANhRqPK7iTp6kuFiY2WtOg90KTMNlIT+Ff85Y2
b1rHj6Z0SrCkJujhWk3IBPic/wJgz01LEc/OAdUPlby90RJZcIBhSlWhT7mXnXIO
c0HWlNQrns2s3CTyYwZSiSlYe9ApeLwhjDo8NhbFuCAy61l6O5UsR4AfZxx/rGKv
2wFb1/RN/P4gNe6vmxZAPjR0AQcwD3tc2McimOLr/22kmPz8IH3I0X7WoSFr0Biz
E91G7bb0hOb/cA==
=knv7
-----END PGP PUBLIC KEY BLOCK-----
`

// dearmor décode une clé publique au format ASCII (gpgv ne lit que le binaire).
func dearmor(asc string) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(asc, "\r\n", "\n"), "\n")
	var body strings.Builder
	in, headers := false, false
	for _, l := range lines {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "-----BEGIN PGP PUBLIC KEY BLOCK-----"):
			in, headers = true, true
		case strings.HasPrefix(l, "-----END PGP PUBLIC KEY BLOCK-----"):
			in = false
		case !in:
		case headers:
			// En-têtes facultatifs (« Version: … »), puis une ligne vide.
			if l == "" {
				headers = false
			} else if !strings.Contains(l, ":") {
				headers = false
				body.WriteString(l)
			}
		case strings.HasPrefix(l, "="): // somme de contrôle CRC-24
		default:
			body.WriteString(l)
		}
	}
	b, err := base64.StdEncoding.DecodeString(body.String())
	if err != nil || len(b) == 0 {
		return nil, errors.New("clé PGP illisible")
	}
	return b, nil
}

// keyFingerprint : empreinte v4 (SHA-1 de 0x99, longueur, corps) du premier paquet,
// qui doit être une clé publique.
func keyFingerprint(bin []byte) (string, error) {
	if len(bin) < 3 || bin[0]&0x80 == 0 {
		return "", errors.New("paquet PGP invalide")
	}
	var tag, start, length int
	if bin[0]&0x40 != 0 { // nouveau format
		tag = int(bin[0] & 0x3f)
		switch l := int(bin[1]); {
		case l < 192:
			start, length = 2, l
		case l < 224 && len(bin) > 2:
			start, length = 3, (l-192)<<8+int(bin[2])+192
		case l == 255 && len(bin) > 5:
			start, length = 6, int(bin[2])<<24|int(bin[3])<<16|int(bin[4])<<8|int(bin[5])
		default:
			return "", errors.New("longueur de paquet PGP non prise en charge")
		}
	} else { // ancien format
		tag = int(bin[0]>>2) & 0x0f
		switch bin[0] & 3 {
		case 0:
			start, length = 2, int(bin[1])
		case 1:
			start, length = 3, int(bin[1])<<8|int(bin[2])
		case 2:
			if len(bin) < 5 {
				return "", errors.New("paquet PGP tronqué")
			}
			start, length = 5, int(bin[1])<<24|int(bin[2])<<16|int(bin[3])<<8|int(bin[4])
		default:
			return "", errors.New("longueur de paquet PGP indéterminée")
		}
	}
	if tag != 6 || start+length > len(bin) || length < 1 || bin[start] != 4 {
		return "", errors.New("clé publique PGP v4 attendue")
	}
	h := sha1.New()
	h.Write([]byte{0x99, byte(length >> 8), byte(length)})
	h.Write(bin[start : start+length])
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

// verifyPGP vérifie la signature détachée sig de file par la clé keyASC, dont
// l'empreinte doit être fpr. gpgv lit la clé (binaire) depuis keyring et renvoie ses
// lignes d'état (--status-fd).
func verifyPGP(file, sig, keyASC, fpr string, gpgv func(keyring, sig, file string) (string, error)) (string, error) {
	key, err := dearmor(keyASC)
	if err != nil {
		return "", err
	}
	if got, err := keyFingerprint(key); err != nil || got != fpr {
		return "", fmt.Errorf("clé de vérification inattendue (%s, %v)", got, err)
	}
	if sig == "" {
		return "", errors.New("signature .sig absente")
	}
	keyring := filepath.Join(filepath.Dir(file), "aws-cli-key.gpg")
	if err := os.WriteFile(keyring, key, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(keyring)
	status, err := gpgv(keyring, sig, file)
	good, valid := false, false
	for _, l := range strings.Split(status, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || f[0] != "[GNUPG:]" {
			continue
		}
		switch f[1] {
		case "GOODSIG":
			good = true
		case "VALIDSIG":
			// VALIDSIG <empreinte de la clé qui signe> … <empreinte de la clé principale>
			valid = f[2] == fpr || f[len(f)-1] == fpr
		case "EXPKEYSIG":
			return "", errors.New("clé de signature AWS expirée : mettre à jour kura-agent")
		case "BADSIG":
			return "", errors.New("signature PGP invalide (fichier modifié ?)")
		}
	}
	if err != nil || !good || !valid {
		return "", fmt.Errorf("signature PGP non reconnue (%v)", err)
	}
	return "AWS CLI Team <aws-cli@amazon.com> (" + fpr[:4] + "…" + fpr[len(fpr)-4:] + ")", nil
}

// runGPGV : gpgv, toujours présent sous Debian et Ubuntu (apt s'en sert).
func runGPGV(keyring, sig, file string) (string, error) {
	cmd := exec.Command("gpgv", "--status-fd", "1", "--keyring", keyring, sig, file)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return "", fmt.Errorf("gpgv : %w (installer gpgv)", err)
	}
	if err != nil {
		err = fmt.Errorf("gpgv : %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), err
}
