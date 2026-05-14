package config_test

import (
	"testing"

	"github.com/parisxmas/OxiMail/internal/config"
	"github.com/parisxmas/OxiMail/internal/itest"
)

func TestTLSConfig(t *testing.T) {
	t.Run("disabled when neither cert nor key is set", func(t *testing.T) {
		tc, err := config.Config{}.TLSConfig()
		if tc != nil || err != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", tc, err)
		}
	})

	t.Run("error when only one of cert/key is set", func(t *testing.T) {
		if _, err := (config.Config{TLSCert: "/x/cert.pem"}).TLSConfig(); err == nil {
			t.Error("cert without key: want an error")
		}
		if _, err := (config.Config{TLSKey: "/x/key.pem"}).TLSConfig(); err == nil {
			t.Error("key without cert: want an error")
		}
	})

	t.Run("loads a valid cert/key pair", func(t *testing.T) {
		certFile, keyFile := itest.WriteSelfSignedCert(t)
		tc, err := config.Config{TLSCert: certFile, TLSKey: keyFile}.TLSConfig()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if tc == nil || len(tc.Certificates) != 1 {
			t.Fatalf("got %+v, want a config with exactly one certificate", tc)
		}
	})

	t.Run("error on missing cert files", func(t *testing.T) {
		_, err := config.Config{TLSCert: "/no/such/cert.pem", TLSKey: "/no/such/key.pem"}.TLSConfig()
		if err == nil {
			t.Error("missing files: want an error")
		}
	})
}
