package restore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Bucket lit les versions d'un bucket Wasabi avec la clé du poste (droits
// ListBucketVersions et, au lot 2, GetObjectVersion). Seules des requêtes de lecture
// partent d'ici.
type Bucket struct {
	Name   string
	client *s3.Client
}

// NewBucket prépare le client : adressage par chemin (noms de bucket avec des points),
// sommes de contrôle seulement quand l'opération l'exige (Wasabi).
func NewBucket(endpoint, region, bucket, accessKeyID, secret string, hc *http.Client) *Bucket {
	if region == "" {
		region = "us-east-1"
	}
	opts := s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: accessKeyID, SecretAccessKey: secret, Source: "kura-agent"}, nil
		}),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           10,
	}
	if hc != nil {
		opts.HTTPClient = hc
	}
	return &Bucket{Name: bucket, client: s3.New(opts)}
}

// ListVersions liste versions et marqueurs de suppression sous prefix, page par page.
func (b *Bucket) ListVersions(ctx context.Context, prefix string) ([]Version, error) {
	in := &s3.ListObjectVersionsInput{Bucket: aws.String(b.Name)}
	if prefix != "" {
		in.Prefix = aws.String(prefix)
	}
	var out []Version
	for page := 0; ; page++ {
		res, err := b.client.ListObjectVersions(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("liste des versions de %s/%s : %w", b.Name, prefix, err)
		}
		for _, v := range res.Versions {
			out = append(out, Version{Key: aws.ToString(v.Key), VersionID: aws.ToString(v.VersionId),
				LastModified: aws.ToTime(v.LastModified), Size: aws.ToInt64(v.Size), Latest: aws.ToBool(v.IsLatest)})
		}
		for _, d := range res.DeleteMarkers {
			out = append(out, Version{Key: aws.ToString(d.Key), VersionID: aws.ToString(d.VersionId),
				LastModified: aws.ToTime(d.LastModified), Latest: aws.ToBool(d.IsLatest), Deleted: true})
		}
		if !aws.ToBool(res.IsTruncated) {
			return out, nil
		}
		if res.NextKeyMarker == nil && res.NextVersionIdMarker == nil {
			return nil, fmt.Errorf("liste des versions de %s/%s : page %d tronquée sans marqueur de suite", b.Name, prefix, page+1)
		}
		in.KeyMarker, in.VersionIdMarker = res.NextKeyMarker, res.NextVersionIdMarker
	}
}

// Object est le contenu d'une version, à lire puis fermer.
type Object struct {
	Body io.ReadCloser
	Size int64
	// MD5 : empreinte de la version quand le bucket la connaît (envoi en une partie :
	// l'ETag est alors le MD5 du contenu) ; vide sinon.
	MD5 string
}

// Getter lit une version précise d'un objet.
type Getter interface {
	GetVersion(ctx context.Context, key, versionID string) (Object, error)
}

// GetVersion lit une version (GetObject avec versionId) : lecture seule.
func (b *Bucket) GetVersion(ctx context.Context, key, versionID string) (Object, error) {
	res, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.Name), Key: aws.String(key),
		VersionId: aws.String(versionID)})
	if err != nil {
		return Object{}, fmt.Errorf("lecture de %s (version %s) : %w", key, versionID, err)
	}
	return Object{Body: res.Body, Size: aws.ToInt64(res.ContentLength), MD5: etagMD5(aws.ToString(res.ETag))}, nil
}

// etagMD5 : l'ETag d'un envoi en une partie est le MD5 du contenu (32 chiffres
// hexadécimaux) ; celui d'un envoi en plusieurs parties porte un tiret et ne l'est pas.
func etagMD5(etag string) string {
	e := strings.ToLower(strings.Trim(etag, `"`))
	if len(e) != 32 {
		return ""
	}
	for _, c := range e {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ""
		}
	}
	return e
}
