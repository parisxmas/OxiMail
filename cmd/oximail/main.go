// Command oximail is the OxiMail entry point: it loads the
// configuration, opens the OxiDB-backed store, wires the protocol
// servers and the outbound queue together, runs them, and shuts them
// down gracefully on SIGINT / SIGTERM.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/parisxmas/OxiMail/internal/config"
	"github.com/parisxmas/OxiMail/internal/imap"
	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/queue"
	"github.com/parisxmas/OxiMail/internal/smtp"
	"github.com/parisxmas/OxiMail/internal/spam"
	"github.com/parisxmas/OxiMail/internal/store"
	"github.com/parisxmas/OxiMail/internal/webmail"
)

// component is a long-running server or worker. Start blocks until the
// context is cancelled or the component fails; Stop triggers a graceful
// shutdown.
type component interface {
	Start(context.Context) error
	Stop() error
}

// named pairs a component with the label used for its log lines.
type named struct {
	name string
	component
}

func main() {
	cfg := config.Load()
	observability.SetupLogging(cfg.LogFormat, cfg.LogLevel)
	log.Printf("starting — hostname=%s oxidb=%s:%d", cfg.Hostname, cfg.OxiDBHost, cfg.OxiDBPort)

	tlsConfig, acmeManager, tlsReloader, err := cfg.TLSConfig()
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	switch {
	case acmeManager != nil:
		log.Printf("TLS via ACME — hosts=%v cache=%s challenge=%s", cfg.ACMEHosts, cfg.ACMECache, cfg.ACMEChallengeAddr)
	case tlsConfig != nil:
		log.Print("TLS configured (static cert) — SIGHUP reloads the PEM files on disk")
	default:
		log.Print("TLS not configured (set OXIMAIL_TLS_CERT/OXIMAIL_TLS_KEY or OXIMAIL_ACME_HOSTS) — running without TLS")
	}

	// Storage — everything sits on OxiDB.
	st, err := store.Open(cfg.OxiDBHost, cfg.OxiDBPort)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	if err := store.EnsureSchema(st); err != nil {
		log.Fatalf("schema: %v", err)
	}

	srsSecret, err := cfg.SRSSecretBytes()
	if err != nil {
		log.Fatalf("srs: %v", err)
	}
	fwd := smtp.ForwarderConfig{
		SRSSecret:       srsSecret,
		SRSMaxAge:       cfg.SRSMaxAge,
		ForwarderDomain: cfg.Hostname,
	}
	if len(srsSecret) == 0 {
		log.Print("SRS not configured (set OXIMAIL_SRS_SECRET) — alias forwarding to remote addresses is disabled")
	}

	// Components, in start order. The implicit-TLS surfaces are only
	// brought up when a certificate is configured.
	pipeline := spam.New(cfg.RspamdURL)
	components := []named{
		{"observability", observability.New(cfg.MetricsAddr, st)},
		{"spam", pipeline},
		{"smtp", smtp.New(cfg.SMTPAddr, cfg.Hostname, st, pipeline, tlsConfig, fwd)},
		{"submission", smtp.NewSubmission(cfg.SubmissionAddr, cfg.Hostname, st, tlsConfig)},
		{"imap", imap.New(cfg.IMAPAddr, st, tlsConfig)},
		{"webmail", webmail.New(cfg.WebmailAddr, cfg.WebmailStatic, st, tlsConfig, webmail.MTASTSPolicy{
			Mode:   cfg.MTASTSMode,
			MX:     mtastsMX(cfg),
			MaxAge: cfg.MTASTSMaxAge,
		})},
	}
	if tlsConfig != nil {
		components = append(components,
			named{"submission-tls", smtp.NewSubmissionTLS(cfg.SMTPSAddr, cfg.Hostname, st, tlsConfig)},
			named{"imap-tls", imap.NewTLS(cfg.IMAPSAddr, st, tlsConfig)},
		)
	}
	if acmeManager != nil {
		// HTTP-01 validation needs a public :80 listener. autocert's
		// handler answers /.well-known/acme-challenge/* and (when
		// passed a non-nil fallback) redirects everything else.
		components = append(components, named{
			"acme-challenge",
			&httpComponent{
				addr:    cfg.ACMEChallengeAddr,
				handler: acmeManager.HTTPHandler(http.HandlerFunc(redirectToHTTPS)),
			},
		})
	}
	components = append(components, named{"queue", queue.New(st, cfg.Hostname)})

	// Run each component until the process is asked to stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SIGHUP reloads what we can without a restart. Today: re-read the
	// static TLS cert from disk so certbot renew (or any out-of-band
	// rotation) takes effect on the next handshake. ACME mode renews
	// itself, so SIGHUP is a no-op there.
	if tlsReloader != nil {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-hup:
					if err := tlsReloader.Reload(); err != nil {
						log.Printf("tls: reload failed (keeping previous cert): %v", err)
					} else {
						log.Print("tls: reloaded certificate from disk")
					}
				}
			}
		}()
	}

	var wg sync.WaitGroup
	run := func(name string, start func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := start(ctx); err != nil {
				log.Printf("%s: %v", name, err)
			}
		}()
	}
	for _, c := range components {
		run(c.name, c.Start)
	}

	<-ctx.Done()
	log.Print("shutdown signal received")
	for _, c := range components {
		_ = c.Stop()
	}
	wg.Wait()
	log.Print("stopped")
}

// httpComponent runs a plain http.Server as one of the lifecycle
// components. It is used for the ACME HTTP-01 challenge listener; the
// other servers manage their own listeners.
type httpComponent struct {
	addr    string
	handler http.Handler

	srv  *http.Server
	once sync.Once
}

func (h *httpComponent) Start(ctx context.Context) error {
	h.srv = &http.Server{
		Addr:              h.addr,
		Handler:           h.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- h.srv.ListenAndServe() }()
	log.Printf("acme-challenge: listening on %s", h.addr)
	select {
	case <-ctx.Done():
		return h.Stop()
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (h *httpComponent) Stop() error {
	h.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if h.srv != nil {
			_ = h.srv.Shutdown(ctx)
		}
		log.Print("acme-challenge: stopped")
	})
	return nil
}

// redirectToHTTPS sends every plaintext request to its HTTPS sibling —
// the fallback for the ACME challenge listener.
func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + r.Host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// mtastsMX returns the MX hostname list for the MTA-STS policy: the
// operator-configured list when present, otherwise just the announced
// hostname.
func mtastsMX(cfg config.Config) []string {
	if len(cfg.MTASTSMX) > 0 {
		return cfg.MTASTSMX
	}
	return []string{cfg.Hostname}
}
