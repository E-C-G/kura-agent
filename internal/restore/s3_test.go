package restore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// faux Wasabi : deux pages de ListObjectVersions, avec un marqueur de suppression.
func fakeVersions(t *testing.T) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/acme-backup/") && r.URL.Query().Has("versionId") {
			switch r.URL.Query().Get("versionId") {
			case "v1":
				w.Header().Set("ETag", `"781e5e245d69b566979b86e28d23f2c7"`) // MD5 de 0123456789
				w.Header().Set("Content-Length", "10")
				fmt.Fprint(w, "0123456789")
			case "multi":
				w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef-3"`)
				fmt.Fprint(w, "abc")
			default:
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `<Error><Code>NoSuchVersion</Code></Error>`)
			}
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/acme-backup" || !r.URL.Query().Has("versions") {
			http.Error(w, "inattendu", http.StatusBadRequest)
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=AKIDTEST/") {
			http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("key-marker") == "" {
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>acme-backup</Name><Prefix>acme-portable01/documents</Prefix>
  <IsTruncated>true</IsTruncated>
  <NextKeyMarker>acme-portable01/documents/a.txt</NextKeyMarker>
  <NextVersionIdMarker>v1</NextVersionIdMarker>
  <Version><Key>acme-portable01/documents/a.txt</Key><VersionId>v2</VersionId><IsLatest>true</IsLatest>
    <LastModified>2026-10-10T08:00:00.000Z</LastModified><Size>12</Size></Version>
  <Version><Key>acme-portable01/documents/a.txt</Key><VersionId>v1</VersionId><IsLatest>false</IsLatest>
    <LastModified>2026-10-01T08:00:00.000Z</LastModified><Size>10</Size></Version>
</ListVersionsResult>`)
			return
		}
		if r.URL.Query().Get("key-marker") != "acme-portable01/documents/a.txt" || r.URL.Query().Get("version-id-marker") != "v1" {
			http.Error(w, "marqueurs", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>acme-backup</Name><IsTruncated>false</IsTruncated>
  <DeleteMarker><Key>acme-portable01/documents/b.txt</Key><VersionId>d1</VersionId><IsLatest>true</IsLatest>
    <LastModified>2026-10-05T08:00:00.000Z</LastModified></DeleteMarker>
  <Version><Key>acme-portable01/documents/b.txt</Key><VersionId>b1</VersionId><IsLatest>false</IsLatest>
    <LastModified>2026-10-01T08:00:00.000Z</LastModified><Size>20</Size></Version>
</ListVersionsResult>`)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestBucketListVersions(t *testing.T) {
	srv, seen := fakeVersions(t)
	b := NewBucket(srv.URL, "eu-central-1", "acme-backup", "AKIDTEST", "secret", srv.Client())
	vs, err := b.ListVersions(context.Background(), "acme-portable01/documents")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 4 || len(*seen) != 2 {
		t.Fatalf("%d versions en %d requêtes : %+v", len(vs), len(*seen), vs)
	}
	if !strings.Contains((*seen)[0], "prefix=acme-portable01%2Fdocuments") {
		t.Errorf("préfixe absent : %s", (*seen)[0])
	}
	var deleted int
	for _, v := range vs {
		if v.Deleted {
			deleted++
			if v.Key != "acme-portable01/documents/b.txt" || !v.Latest {
				t.Errorf("marqueur : %+v", v)
			}
		}
	}
	if deleted != 1 {
		t.Errorf("%d marqueurs", deleted)
	}
	files := StateAt(vs, Target{Key: "acme-portable01/documents", Root: true}, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), false)
	if len(files) != 2 || files[1].DeletedSince == nil || files[0].Version.VersionID != "v1" {
		t.Errorf("état au 02/10 : %+v", files)
	}
}

func TestBucketDenied(t *testing.T) {
	srv, _ := fakeVersions(t)
	b := NewBucket(srv.URL, "", "acme-backup", "AUTRE", "secret", srv.Client())
	if _, err := b.ListVersions(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("erreur attendue : %v", err)
	}
}

func TestBucketGetVersion(t *testing.T) {
	srv, seen := fakeVersions(t)
	b := NewBucket(srv.URL, "eu-central-1", "acme-backup", "AKIDTEST", "secret", srv.Client())
	o, err := b.GetVersion(context.Background(), "acme-portable01/documents/a.txt", "v1")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(o.Body)
	o.Body.Close()
	if string(data) != "0123456789" || o.Size != 10 || o.MD5 != "781e5e245d69b566979b86e28d23f2c7" {
		t.Fatalf("%q %d %q", data, o.Size, o.MD5)
	}
	o, err = b.GetVersion(context.Background(), "acme-portable01/documents/gros.bin", "multi")
	if err != nil || o.MD5 != "" {
		t.Fatalf("envoi en plusieurs parties : empreinte %q, %v", o.MD5, err)
	}
	o.Body.Close()
	if _, err := b.GetVersion(context.Background(), "acme-portable01/documents/x", "absente"); err == nil || !strings.Contains(err.Error(), "NoSuchVersion") {
		t.Fatalf("version absente : %v", err)
	}
	if _, err := b.ListVersions(context.Background(), "acme-portable01/documents"); err != nil {
		t.Fatal(err)
	}
	// lecture seule : aucune requête d'écriture, même après une erreur
	for _, r := range *seen {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("requête d'écriture : %s", r)
		}
	}
	if !strings.Contains((*seen)[0], "versionId=v1") {
		t.Errorf("version non demandée : %s", (*seen)[0])
	}
}

func TestEtagMD5(t *testing.T) {
	for etag, want := range map[string]string{
		`"C7B5C3B1F1E9A1AB3D4A3A0F1BB31B7E"`:   "c7b5c3b1f1e9a1ab3d4a3a0f1bb31b7e",
		`"c7b5c3b1f1e9a1ab3d4a3a0f1bb31b7e-2"`: "",
		"":                                     "",
		`"zzb5c3b1f1e9a1ab3d4a3a0f1bb31b7e"`:   "",
	} {
		if got := etagMD5(etag); got != want {
			t.Errorf("%s : %q", etag, got)
		}
	}
}
