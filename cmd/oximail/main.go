// Command oximail is the OxiMail entry point: it loads the
// configuration, opens the OxiDB-backed store, wires the protocol
// servers and the outbound queue together, runs them, and shuts them
// down gracefully on SIGINT / SIGTERM.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/parisxmas/OxiMail/internal/config"
	"github.com/parisxmas/OxiMail/internal/imap"
	"github.com/parisxmas/OxiMail/internal/queue"
	"github.com/parisxmas/OxiMail/internal/smtp"
	"github.com/parisxmas/OxiMail/internal/spam"
	"github.com/parisxmas/OxiMail/internal/store"
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
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("oximail: ")

	cfg := config.Load()
	log.Printf("starting — hostname=%s oxidb=%s:%d", cfg.Hostname, cfg.OxiDBHost, cfg.OxiDBPort)

	tlsConfig, err := cfg.TLSConfig()
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	if tlsConfig == nil {
		log.Print("TLS not configured (set OXIMAIL_TLS_CERT and OXIMAIL_TLS_KEY) — running without TLS")
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

	// Components, in start order. The implicit-TLS surfaces are only
	// brought up when a certificate is configured.
	pipeline := spam.New(cfg.RspamdURL)
	components := []named{
		{"smtp", smtp.New(cfg.SMTPAddr, cfg.Hostname, st, pipeline, tlsConfig)},
		{"submission", smtp.NewSubmission(cfg.SubmissionAddr, cfg.Hostname, st, tlsConfig)},
		{"imap", imap.New(cfg.IMAPAddr, st, tlsConfig)},
	}
	if tlsConfig != nil {
		components = append(components,
			named{"submission-tls", smtp.NewSubmissionTLS(cfg.SMTPSAddr, cfg.Hostname, st, tlsConfig)},
			named{"imap-tls", imap.NewTLS(cfg.IMAPSAddr, st, tlsConfig)},
		)
	}
	components = append(components, named{"queue", queue.New(st, cfg.Hostname)})

	// Run each component until the process is asked to stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
