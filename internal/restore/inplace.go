package restore

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
)

// Action : ce qu'une restauration en place fait d'un fichier.
type Action int

const (
	ActWrite   Action = iota // absent : remis
	ActReplace               // présent et différent, --replace : remplacé, l'actuel gardé à côté
	ActKeep                  // présent, sans --replace : laissé
	ActSame                  // présent et identique à la version : laissé
)

// partSize : taille des parties d'un envoi de la CLI AWS (multipart_chunksize par
// défaut), pour recalculer l'ETag d'un gros fichier.
const partSize = 8 << 20

// Classify dit ce que la restauration en place ferait du fichier it.Out sous la
// destination. Avec replace, un fichier présent est comparé à la version (taille puis
// empreinte) ; sans, il est laissé tel quel.
func (d *Dest) Classify(it Item, replace bool) (Action, error) {
	fi, err := d.root.Lstat(it.Out)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ActWrite, nil
	case err != nil:
		return 0, err
	case isLink(fi):
		return 0, ErrLink
	case !fi.Mode().IsRegular():
		return 0, errors.New("un dossier occupe la place du fichier")
	case !replace:
		return ActKeep, nil
	case fi.Size() != it.Version.Size:
		return ActReplace, nil
	}
	same, err := d.sameContent(it.Out, it.Version)
	if err != nil {
		return 0, fmt.Errorf("fichier actuel illisible (ouvert par une application ?) : %w", err)
	}
	if same {
		return ActSame, nil
	}
	return ActReplace, nil
}

// sameContent compare le fichier à l'ETag de la version : MD5 du contenu pour un envoi
// en une partie, MD5 des MD5 des parties de 8 Mio suivi de -N pour un envoi en
// plusieurs. Un ETag d'une autre forme (autre taille de partie) compte comme différent :
// le fichier est alors remplacé, l'actuel gardé à côté.
func (d *Dest) sameContent(rel string, v Version) (bool, error) {
	etag := strings.ToLower(strings.Trim(v.ETag, `"`))
	parts := 0
	if i := strings.IndexByte(etag, '-'); i > 0 {
		n, err := strconv.Atoi(etag[i+1:])
		if err != nil || n < 1 || int64(n) != (v.Size+partSize-1)/partSize {
			return false, nil
		}
		parts, etag = n, etag[:i]
	}
	if len(etag) != 32 {
		return false, nil
	}
	f, err := d.root.Open(rel)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if parts == 0 {
		h := md5.New()
		if _, err := io.Copy(h, f); err != nil {
			return false, err
		}
		return hex.EncodeToString(h.Sum(nil)) == etag, nil
	}
	all := md5.New()
	for range parts {
		h := md5.New()
		if _, err := io.CopyN(h, f, partSize); err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		all.Write(h.Sum(nil))
	}
	return hex.EncodeToString(all.Sum(nil)) == etag, nil
}
