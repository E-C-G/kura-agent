package awscli

import (
	"strings"
	"testing"

	"github.com/E-C-G/kura-agent/pkg/config"
)

var profile = config.Profile{
	Endpoint:          "https://s3.eu-central-1.wasabisys.com",
	Bucket:            "acme-srv01",
	ChecksumAlgorithm: "CRC32",
}

// La commande générée doit reproduire le script planifié actuel.
func TestSyncArgsMatchCurrentScript(t *testing.T) {
	data := config.Job{
		ID: "data", Source: "E:/", Destination: "DATA", Delete: true,
		Exclude: []string{"*$RECYCLE.BIN*", "*System Volume Information*"},
	}
	got := strings.Join(SyncArgs(profile, data, false), " ")
	want := "s3 sync E:/ s3://acme-srv01/DATA --endpoint-url https://s3.eu-central-1.wasabisys.com --no-progress " +
		"--exclude *$RECYCLE.BIN* --exclude *System Volume Information* --delete"
	if got != want {
		t.Errorf("commande :\n reçu   %s\n attendu %s", got, want)
	}

	outlook := config.Job{ID: "outlook", Source: "C:/Users/alice/AppData/Local/Microsoft/Outlook", Destination: "C/alice/Outlook", Delete: true}
	got = strings.Join(SyncArgs(profile, outlook, true), " ")
	if !strings.Contains(got, "s3://acme-srv01/C/alice/Outlook") || !strings.HasSuffix(got, "--delete --dryrun") {
		t.Errorf("commande outlook en simulation inattendue : %s", got)
	}
}

func TestEnvNeverPagesAndSetsProfile(t *testing.T) {
	env := Env(config.Profile{AWSProfile: "wasabi"})
	if strings.Join(env, " ") != "AWS_PAGER= PYTHONUTF8=1 PYTHONIOENCODING=utf-8 "+
		"AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED AWS_PROFILE=wasabi" {
		t.Errorf("environnement inattendu : %v", env)
	}
}

func TestClassify(t *testing.T) {
	cases := map[int]Outcome{0: OK, 2: Warning, 1: Retryable, 254: Retryable, 255: Retryable, 252: Failed, 253: Failed, 130: Failed}
	for code, want := range cases {
		if got := Classify(code); got != want {
			t.Errorf("Classify(%d) = %v, attendu %v", code, got, want)
		}
	}
}

func TestCounterCountsAcrossChunks(t *testing.T) {
	var sink strings.Builder
	c := NewCounter(&sink)
	chunks := []string{
		"upload: E:\\a.txt to s3://acme-srv01/DATA/a.txt\nupl",
		"oad: E:\\b.txt to s3://acme-srv01/DATA/b.txt\n(dryrun) delete: s3://acme-srv01/DATA/old.txt\n",
		"warning: Skipping file E:\\locked.pst. File/Directory is not readable.\n",
		"delete: s3://acme-srv01/DATA/x.txt", // dernière ligne sans saut de ligne
	}
	for _, ch := range chunks {
		c.Write([]byte(ch))
	}
	got := c.Counts()
	if got != (Counts{Uploaded: 2, Deleted: 2, Skipped: 1}) {
		t.Errorf("comptes = %+v", got)
	}
	if sink.String() != strings.Join(chunks, "") {
		t.Error("la sortie doit être recopiée telle quelle dans le journal")
	}
}
