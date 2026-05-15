package config_test

import (
	"testing"

	"github.com/parisxmas/OxiMail/internal/config"
	"github.com/parisxmas/OxiMail/internal/itest"
)

func TestTLSConfig(t *testing.T) {
	t.Run("disabled when neither cert nor key is set", func(t *testing.T) {
		tc, m, err := config.Config{}.TLSConfig()
		if tc != nil || m != nil || err != nil {
			t.Fatalf("got (%v, %v, %v), want (nil, nil, nil)", tc, m, err)
		}
	})

	t.Run("error when only one of cert/key is set", func(t *testing.T) {
		if _, _, err := (config.Config{TLSCert: "/x/cert.pem"}).TLSConfig(); err == nil {
			t.Error("cert without key: want an error")
		}
		if _, _, err := (config.Config{TLSKey: "/x/key.pem"}).TLSConfig(); err == nil {
			t.Error("key without cert: want an error")
		}
	})

	t.Run("loads a valid cert/key pair", func(t *testing.T) {
		certFile, keyFile := itest.WriteSelfSignedCert(t)
		tc, m, err := config.Config{TLSCert: certFile, TLSKey: keyFile}.TLSConfig()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if tc == nil || len(tc.Certificates) != 1 {
			t.Fatalf("got %+v, want a config with exactly one certificate", tc)
		}
		if m != nil {
			t.Error("static-cert path returned an autocert.Manager; want nil")
		}
	})

	t.Run("error on missing cert files", func(t *testing.T) {
		_, _, err := config.Config{TLSCert: "/no/such/cert.pem", TLSKey: "/no/such/key.pem"}.TLSConfig()
		if err == nil {
			t.Error("missing files: want an error")
		}
	})

	t.Run("ACME mode returns an autocert manager and ignores static files", func(t *testing.T) {
		cfg := config.Config{
			TLSCert:   "/should/be/ignored",
			TLSKey:    "/should/be/ignored",
			ACMEHosts: []string{"mail.example.test"},
			ACMECache: t.TempDir(),
			ACMEEmail: "ops@example.test",
		}
		tc, m, err := cfg.TLSConfig()
		if err != nil {
			t.Fatalf("ACME mode: %v", err)
		}
		if m == nil {
			t.Fatal("ACME mode did not return an autocert.Manager")
		}
		if tc == nil || tc.GetCertificate == nil {
			t.Fatalf("ACME tls.Config = %+v, want a non-nil config with GetCertificate", tc)
		}
		if len(tc.Certificates) != 0 {
			t.Errorf("ACME tls.Config has %d static certs, want 0 (the static files are meant to be ignored)", len(tc.Certificates))
		}
	})

	t.Run("ACME staging directory URL is applied", func(t *testing.T) {
		const staging = "https://acme-staging-v02.api.letsencrypt.org/directory"
		cfg := config.Config{
			ACMEHosts:        []string{"mail.example.test"},
			ACMECache:        t.TempDir(),
			ACMEDirectoryURL: staging,
		}
		_, m, err := cfg.TLSConfig()
		if err != nil {
			t.Fatalf("ACME staging: %v", err)
		}
		if m == nil || m.Client == nil || m.Client.DirectoryURL != staging {
			t.Fatalf("ACME directory URL not propagated to the manager: %+v", m)
		}
	})
}
