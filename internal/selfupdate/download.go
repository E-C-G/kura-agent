package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// MaxAssetSize : archive plus grande refusée avant tout téléchargement.
const MaxAssetSize = 100 << 20

// Download télécharge l'archive a dans dst et vérifie son empreinte : celle de a,
// déjà trouvée dans l'attestation de release par Check. Fichier incomplet ou
// d'empreinte différente : supprimé, erreur.
func (c *Client) Download(ctx context.Context, a Asset, dst string) error {
	if a.Size <= 0 || a.Size > MaxAssetSize {
		return fmt.Errorf("%s : taille %d refusée (100 Mo au plus)", a.Name, a.Size)
	}
	if !isSHA256(a.SHA256) {
		return fmt.Errorf("%s : empreinte inconnue", a.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("%s : adresse non HTTPS refusée", a.Name)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "kura-agent"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/octet-stream")
	// Le délai de 30 s du client couvre une réponse d'API, pas une archive de 20 Mo
	// sur une ligne lente : ici, le contexte borne la durée.
	hc := *c.client()
	hc.Timeout = 0
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
		req = req.WithContext(ctx)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("téléchargement de %s : %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("téléchargement de %s : réponse %d", a.Name, resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(dst)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, a.Size+1))
	switch {
	case err != nil:
		return fmt.Errorf("téléchargement de %s : %w", a.Name, err)
	case n != a.Size:
		return fmt.Errorf("téléchargement de %s : %d octets reçus, %d annoncés", a.Name, n, a.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return fmt.Errorf("%s : empreinte %s, %s attestée : archive refusée", a.Name, got, a.SHA256)
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
