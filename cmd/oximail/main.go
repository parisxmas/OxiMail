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

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("oximail: ")

	cfg := config.Load()
	log.Printf("starting — hostname=%s oxidb=%s:%d", cfg.Hostname, cfg.OxiDBHost, cfg.OxiDBPort)

	// Storage — everything sits on OxiDB.
	st, err := store.Open(cfg.OxiDBHost, cfg.OxiDBPort)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()
	if err := store.EnsureSchema(st); err != nil {
		log.Fatalf("schema: %v", err)
	}

	// Components.
	pipeline := spam.New(cfg.RspamdURL)
	smtpSrv := smtp.New(cfg.SMTPAddr, cfg.Hostname, st, pipeline)
	imapSrv := imap.New(cfg.IMAPAddr, st)
	outQueue := queue.New(st)
	// TODO: submission server on cfg.SubmissionAddr (authenticated send).

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
	run("smtp", smtpSrv.Start)
	run("imap", imapSrv.Start)
	run("queue", outQueue.Start)

	<-ctx.Done()
	log.Print("shutdown signal received")
	_ = smtpSrv.Stop()
	_ = imapSrv.Stop()
	_ = outQueue.Stop()
	wg.Wait()
	log.Print("stopped")
}
