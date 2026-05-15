package config

import (
	"crypto/tls"
	"fmt"
	"sync"
)

// TLSReloader holds a static certificate / key pair on disk and
// re-reads it on demand. The point is to pick up a rotated certificate
// (`certbot renew`, manual rotation, anything that overwrites the PEM
// files) without restarting the server — handshakes after a Reload
// use the new cert immediately.
//
// The static-cert branch of Config.TLSConfig wires a Reloader through
// the returned *tls.Config's GetCertificate hook; the main process
// triggers a Reload on SIGHUP, and operators can do the same with
// `pkill -HUP oximail`.
type TLSReloader struct {
	certFile, keyFile string

	mu   sync.RWMutex
	cert *tls.Certificate
}

// NewTLSReloader loads the cert/key pair once and returns a reloader
// that will hand it to TLS handshakes. An error here means the initial
// load failed — the caller can decide whether to fail-fast or fall
// back.
func NewTLSReloader(certFile, keyFile string) (*TLSReloader, error) {
	r := &TLSReloader{certFile: certFile, keyFile: keyFile}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate is the hook for *tls.Config. It returns the currently
// loaded certificate; a concurrent Reload swaps a new one in under
// r.mu.
func (r *TLSReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cert == nil {
		return nil, fmt.Errorf("tlsreloader: no certificate loaded")
	}
	return r.cert, nil
}

// Reload re-reads the PEM files from disk. On failure the previously
// loaded certificate stays in place, so a botched rotation does not
// take TLS offline.
func (r *TLSReloader) Reload() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("tlsreloader: load %s + %s: %w", r.certFile, r.keyFile, err)
	}
	r.mu.Lock()
	r.cert = &cert
	r.mu.Unlock()
	return nil
}
