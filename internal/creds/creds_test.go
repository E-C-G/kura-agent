package creds

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSetGetListDelete(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "credentials")}
	if err := s.Set("webhook", "jeton-secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("hc-url", "https://hc-ping.com/abc"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get("webhook")
	if err != nil || v != "jeton-secret" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if names, _ := s.List(); !slices.Equal(names, []string{"hc-url", "webhook"}) {
		t.Errorf("List = %v", names)
	}
	if err := s.Set("webhook", "nouveau"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get("webhook"); v != "nouveau" {
		t.Errorf("remplacement : %q", v)
	}
	if err := s.Delete("webhook"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("webhook"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "credentials set webhook") {
		t.Errorf("secret supprimé : ErrNotFound avec la commande à lancer attendu, reçu %v", err)
	}
	if err := s.Delete("webhook"); err != nil {
		t.Errorf("supprimer un secret absent n'est pas une erreur : %v", err)
	}
}

func TestFileIsPrivateAndNamesAreChecked(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	for _, bad := range []string{"", "Majuscule", "../evasion", "a b"} {
		if err := s.Set(bad, "x"); err == nil {
			t.Errorf("nom %q accepté", bad)
		}
	}
	if err := s.Set("ok", ""); err == nil {
		t.Error("un secret vide doit être refusé")
	}
	if err := s.Set("ok", "valeur"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(s.Dir, "ok"+ext))
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '/' && st.Mode().Perm() != 0o600 {
		t.Errorf("droits du fichier : %v, attendu 0600", st.Mode().Perm())
	}
}

func TestResolve(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	s.Set("hc-url", "https://hc-ping.com/secret")
	if v, err := s.Resolve("https://hc-ping.com/en-clair"); err != nil || v != "https://hc-ping.com/en-clair" {
		t.Errorf("valeur littérale : %q, %v", v, err)
	}
	if v, err := s.Resolve("credential:hc-url"); err != nil || v != "https://hc-ping.com/secret" {
		t.Errorf("référence : %q, %v", v, err)
	}
	if _, err := s.Resolve("credential:absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("référence absente : %v", err)
	}
}

func TestAWSKeys(t *testing.T) {
	v, err := JoinAWSKeys(" AKIDEXAMPLE ", "s3cr3t/Key+x\n")
	if err != nil || v != "AKIDEXAMPLE:s3cr3t/Key+x" {
		t.Fatalf("JoinAWSKeys = %q, %v", v, err)
	}
	k, err := ParseAWSKeys(v)
	if err != nil || k.AccessKeyID != "AKIDEXAMPLE" || k.SecretAccessKey != "s3cr3t/Key+x" {
		t.Fatalf("ParseAWSKeys = %+v, %v", k, err)
	}
	env := strings.Join(k.Env(), "\n")
	for _, want := range []string{"AWS_ACCESS_KEY_ID=AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY=s3cr3t/Key+x", "AWS_SESSION_TOKEN="} {
		if !strings.Contains(env, want) {
			t.Errorf("Env sans %q : %s", want, env)
		}
	}
	for _, bad := range [][2]string{{"", "x"}, {"id", ""}, {"a:b", "x"}, {"id", "a b"}} {
		if _, err := JoinAWSKeys(bad[0], bad[1]); err == nil {
			t.Errorf("JoinAWSKeys(%q, %q) accepté", bad[0], bad[1])
		}
	}
	for _, bad := range []string{"", "seul", ":x", "x:"} {
		if _, err := ParseAWSKeys(bad); err == nil {
			t.Errorf("ParseAWSKeys(%q) accepté", bad)
		}
	}
}
