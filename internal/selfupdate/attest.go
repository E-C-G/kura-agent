package selfupdate

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// githubTrustedRoot : racine de confiance des attestations de GitHub (autorités de
// certificats et d'horodatage), telle que la publie le dépôt TUF de GitHub
// (gh attestation trusted-root). Contrôlée chaque semaine en CI
// (.github/workflows/trust-root.yml) : GitHub ajoute une autorité tous les six mois
// environ ; un agent qui n'a pas la nouvelle ne peut plus vérifier les releases
// signées par elle et se met à jour une fois à la main.
//
//go:embed github_trusted_root.json
var githubTrustedRoot []byte

// GitHubTrustedRoot renvoie la racine embarquée (pour la CI et les diagnostics).
func GitHubTrustedRoot() []byte { return githubTrustedRoot }

const (
	// Identité du certificat des attestations de release de github.com.
	releaseSAN = `^https://dotcom\.releases\.github\.com$`
	// Type de prédicat des attestations de release (v0.1, v0.2…).
	releasePredicatePrefix = "https://in-toto.io/attestation/release/"
)

type apiAttestations struct {
	Attestations []struct {
		Bundle    json.RawMessage `json:"bundle"`
		Initiator string          `json:"initiator"`
	} `json:"attestations"`
}

// verifyAsset récupère l'attestation de release qui couvre l'empreinte de l'archive
// et la vérifie : signature GitHub, puis dépôt, tag, nom et empreinte de l'archive.
func (c *Client) verifyAsset(ctx context.Context, rel Release, a Asset) error {
	var list apiAttestations
	path := "/repos/" + Repo + "/attestations/sha256:" + a.SHA256 + "?predicate_type=release&per_page=30"
	if err := c.getJSON(ctx, path, maxAttestationJSON, &list); err != nil {
		return fmt.Errorf("attestation de %s : %w", a.Name, err)
	}
	trusted := c.TrustedRoot
	if len(trusted) == 0 {
		trusted = githubTrustedRoot
	}
	tr, err := root.NewTrustedRootFromJSON(trusted)
	if err != nil {
		return fmt.Errorf("racine de confiance illisible : %w", err)
	}
	var reasons []string
	for _, att := range list.Attestations {
		if att.Initiator != "github" || len(att.Bundle) == 0 || string(att.Bundle) == "null" {
			continue // seules comptent les attestations émises par GitHub lui-même
		}
		err := VerifyReleaseBundle(tr, att.Bundle, rel.Tag, a.Name, a.SHA256)
		if err == nil {
			return nil
		}
		reasons = append(reasons, err.Error())
	}
	if len(reasons) == 0 {
		return fmt.Errorf("attestation de %s : aucune attestation de release de GitHub ; release publiée sans le réglage « releases immuables » ?", a.Name)
	}
	return fmt.Errorf("attestation de %s invalide : %s", a.Name, strings.Join(reasons, " ; "))
}

// VerifyReleaseBundle vérifie une attestation de release (bundle Sigstore JSON) :
// signature et horodatage contre tr, identité de GitHub, puis contenu : dépôt Repo,
// tag, et un sujet portant le nom et l'empreinte SHA-256 attendus.
func VerifyReleaseBundle(tr *root.TrustedRoot, bundleJSON []byte, tag, name, sha256hex string) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("bundle illisible : %w", err)
	}
	digest, err := hex.DecodeString(sha256hex)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("empreinte SHA-256 invalide %q", sha256hex)
	}
	// Horodatage signé par l'autorité de GitHub (pas de journal de transparence pour
	// les attestations de github.com), comme gh release verify.
	v, err := verify.NewVerifier(tr, verify.WithSignedTimestamps(1))
	if err != nil {
		return err
	}
	san, err := verify.NewSANMatcher("", releaseSAN)
	if err != nil {
		return err
	}
	issuer, err := verify.NewIssuerMatcher("", ".*")
	if err != nil {
		return err
	}
	id, err := verify.NewCertificateIdentity(san, issuer, certificate.Extensions{})
	if err != nil {
		return err
	}
	res, err := v.Verify(&b, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id)))
	if err != nil {
		return fmt.Errorf("signature : %w", err)
	}
	st := res.Statement
	if st == nil {
		return fmt.Errorf("attestation sans déclaration in-toto")
	}
	if !strings.HasPrefix(st.GetPredicateType(), releasePredicatePrefix) {
		return fmt.Errorf("type d'attestation %q : attestation de release attendue", st.GetPredicateType())
	}
	fields := st.GetPredicate().GetFields()
	if got := fields["repository"].GetStringValue(); got != Repo {
		return fmt.Errorf("attestation du dépôt %q, %q attendu", got, Repo)
	}
	if got := fields["tag"].GetStringValue(); got != tag {
		return fmt.Errorf("attestation du tag %q, %q attendu", got, tag)
	}
	for _, s := range st.GetSubject() {
		if s.GetName() == name && strings.EqualFold(s.GetDigest()["sha256"], sha256hex) {
			return nil
		}
	}
	return fmt.Errorf("%s (sha256 %s) absent de l'attestation", name, sha256hex)
}
