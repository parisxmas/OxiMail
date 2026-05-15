package config_test

import (
	"os"
	"testing"

	"github.com/parisxmas/OxiMail/internal/config"
	"github.com/parisxmas/OxiMail/internal/itest"
)

func TestTLSConfig(t *testing.T) {
	t.Run("disabled when neither cert nor key is set", func(t *testing.T) {
		tc, m, r, err := config.Config{}.TLSConfig()
		if tc != nil || m != nil || r != nil || err != nil {
			t.Fatalf("got (%v, %v, %v, %v), want (nil, nil, nil, nil)", tc, m, r, err)
		}
	})

	t.Run("error when only one of cert/key is set", func(t *testing.T) {
		if _, _, _, err := (config.Config{TLSCert: "/x/cert.pem"}).TLSConfig(); err == nil {
			t.Error("cert without key: want an error")
		}
		if _, _, _, err := (config.Config{TLSKey: "/x/key.pem"}).TLSConfig(); err == nil {
			t.Error("key without cert: want an error")
		}
	})

	t.Run("static cert returns a reloader and GetCertificate is wired", func(t *testing.T) {
		certFile, keyFile := itest.WriteSelfSignedCert(t)
		tc, m, r, err := config.Config{TLSCert: certFile, TLSKey: keyFile}.TLSConfig()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if tc == nil || tc.GetCertificate == nil {
			t.Fatalf("static-cert tls.Config = %+v, want a GetCertificate hook", tc)
		}
		if len(tc.Certificates) != 0 {
			t.Errorf("got %d static certs in tls.Config, want 0 (the reloader serves them)", len(tc.Certificates))
		}
		if m != nil {
			t.Error("static-cert path returned an autocert.Manager; want nil")
		}
		if r == nil {
			t.Fatal("static-cert path returned a nil reloader; want non-nil")
		}
		// GetCertificate must return the freshly-loaded cert.
		cert, err := tc.GetCertificate(nil)
		if err != nil || cert == nil {
			t.Fatalf("GetCertificate: cert=%v err=%v", cert, err)
		}
	})

	t.Run("error on missing cert files", func(t *testing.T) {
		_, _, _, err := config.Config{TLSCert: "/no/such/cert.pem", TLSKey: "/no/such/key.pem"}.TLSConfig()
		if err == nil {
			t.Error("missing files: want an error")
		}
	})

	t.Run("reloader picks up a new cert on Reload", func(t *testing.T) {
		certFile, keyFile := itest.WriteSelfSignedCert(t)
		_, _, r, err := config.Config{TLSCert: certFile, TLSKey: keyFile}.TLSConfig()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		first, err := r.GetCertificate(nil)
		if err != nil {
			t.Fatalf("first GetCertificate: %v", err)
		}
		// Rotate: write fresh PEM files to the same paths and reload.
		newCert, newKey := itest.WriteSelfSignedCert(t)
		if err := copyFile(certFile, newCert); err != nil {
			t.Fatalf("rotate cert: %v", err)
		}
		if err := copyFile(keyFile, newKey); err != nil {
			t.Fatalf("rotate key: %v", err)
		}
		if err := r.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		second, err := r.GetCertificate(nil)
		if err != nil {
			t.Fatalf("second GetCertificate: %v", err)
		}
		if first == second {
			t.Error("Reload did not replace the cached certificate pointer")
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
		tc, m, r, err := cfg.TLSConfig()
		if err != nil {
			t.Fatalf("ACME mode: %v", err)
		}
		if m == nil {
			t.Fatal("ACME mode did not return an autocert.Manager")
		}
		if r != nil {
			t.Error("ACME mode returned a TLSReloader; want nil (renewals are autocert's job)")
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
		_, m, _, err := cfg.TLSConfig()
		if err != nil {
			t.Fatalf("ACME staging: %v", err)
		}
		if m == nil || m.Client == nil || m.Client.DirectoryURL != staging {
			t.Fatalf("ACME directory URL not propagated to the manager: %+v", m)
		}
	})
}

// copyFile rewrites dst's contents with src's.
func copyFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}
