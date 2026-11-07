// Package smtp provides OxiMail's two SMTP listeners — the inbound MX
// (New) and the mail submission server (NewSubmission) — both built on
// emersion/go-smtp and sharing the lifecycle in this file.
//
// The inbound MX accepts mail on port 25, runs the spam pipeline on each
// message, resolves recipients against the store, and files accepted
// messages into the right mailboxes. It does not relay — a recipient
// that is not a local mailbox (or an alias to one) is rejected, the one
// exception being a bounce to a valid SRS address, which goes back to
// the remote sender it was rewritten from. The submission server
// (submission.go) requires SMTP AUTH and relays.
//
// When a TLS configuration is supplied, STARTTLS is advertised on the
// plaintext listeners; NewSubmissionTLS additionally serves implicit
// TLS (SMTPS).
package smtp

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/mail"
	"strings"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"github.com/parisxmas/OxiMail/internal/arc"
	"github.com/parisxmas/OxiMail/internal/av"
	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/sieve"
	"github.com/parisxmas/OxiMail/internal/spam"
	"github.com/parisxmas/OxiMail/internal/srs"
	"github.com/parisxmas/OxiMail/internal/store"
	"github.com/parisxmas/OxiMail/internal/vacation"
)

// Server tuning. These are conservative defaults; they move into
// internal/config once they need to be operator-tunable.
const (
	readTimeout     = 5 * time.Minute
	writeTimeout    = 5 * time.Minute
	shutdownTimeout = 10 * time.Second
	maxRecipients   = 100
	// maxMessageBytes caps an accepted message. Bodies up to this size go
	// through OxiDB's native PutObject; raising it well past the OxiDB
	// wire-frame limit will require routing bodies via the S3 blob path.
	maxMessageBytes = 25 << 20 // 25 MiB
	// sweepInterval is how often in-memory per-sender state is expired.
	sweepInterval = time.Hour
	// defaultSuppressWindow applies to a vacation rule that does not
	// set its own SuppressDays.
	defaultSuppressWindow = 7 * 24 * time.Hour
	// junkFolder receives quarantined mail.
	junkFolder = "Junk"
)

// Server is an SMTP listener — either the inbound MX (New) or the
// submission server (NewSubmission / NewSubmissionTLS). They share this
// lifecycle; they differ only in their go-smtp backend and in whether
// the listener is plaintext (with optional STARTTLS) or implicit TLS.
type Server struct {
	name        string // "smtp", "submission", "submission-tls" — for log lines
	addr        string
	srv         *gosmtp.Server
	implicitTLS bool // serve TLS from the first byte, rather than STARTTLS-on-plaintext

	// sweep expires in-memory state every sweepEvery while the server
	// runs. It is nil for a server that keeps none.
	sweep      func(now time.Time)
	sweepEvery time.Duration
}

// newServer builds a go-smtp server with OxiMail's shared tuning, for
// the given backend. A non-nil tlsConfig enables STARTTLS (and, for the
// implicit-TLS variants, the encrypted listener).
func newServer(addr, hostname string, be gosmtp.Backend, tlsConfig *tls.Config) *gosmtp.Server {
	srv := gosmtp.NewServer(be)
	srv.Addr = addr
	srv.Domain = hostname
	srv.TLSConfig = tlsConfig
	srv.ReadTimeout = readTimeout
	srv.WriteTimeout = writeTimeout
	srv.MaxMessageBytes = maxMessageBytes
	srv.MaxRecipients = maxRecipients
	srv.ErrorLog = log.Default()
	return srv
}

// ForwarderConfig carries the operator-tunable knobs for SRS-based
// alias forwarding. A zero value disables forwarding: aliases that
// resolve to remote addresses are not relayed and the MX behaves as
// before (local-only delivery).
type ForwarderConfig struct {
	// SRSSecret signs and verifies the rewritten sender. It must be at
	// least 16 bytes long and stable across restarts — losing it
	// invalidates every outstanding bounce address.
	SRSSecret []byte
	// SRSMaxAge is how long an SRS-encoded bounce address stays
	// valid. Zero means "no age check"; default 21 days is sensible.
	SRSMaxAge time.Duration
	// ForwarderDomain is the domain that hosts the rewritten sender —
	// usually the MX's own hostname. Bounces come back here.
	ForwarderDomain string
}

// New builds the inbound SMTP (MX) server bound to `addr`, announcing
// `hostname` in its greeting. A non-nil tlsConfig advertises STARTTLS.
// `fwd` enables SRS-based alias forwarding to remote addresses; pass
// the zero value to disable it.
func New(addr, hostname string, st *store.Store, sp *spam.Pipeline, avClient *av.Client, tlsConfig *tls.Config, fwd ForwarderConfig) *Server {
	if fwd.ForwarderDomain == "" {
		fwd.ForwarderDomain = hostname
	}
	be := newBackend(hostname, st, sp, avClient, fwd)
	return &Server{
		name:       "smtp",
		addr:       addr,
		srv:        newServer(addr, hostname, be, tlsConfig),
		sweep:      be.sweep,
		sweepEvery: sweepInterval,
	}
}

// Start listens and serves until `ctx` is cancelled, then shuts the
// server down gracefully.
func (s *Server) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.sweep != nil {
		go runSweeper(ctx, s.sweepEvery, s.sweep)
	}

	errc := make(chan error, 1)
	go func() {
		var err error
		if s.implicitTLS {
			err = s.srv.ListenAndServeTLS()
		} else {
			err = s.srv.ListenAndServe()
		}
		if errors.Is(err, gosmtp.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	log.Printf("%s: listening on %s", s.name, s.addr)
	select {
	case <-ctx.Done():
		return s.Stop()
	case err := <-errc:
		return err
	}
}

// runSweeper calls sweep every interval until ctx is cancelled.
func runSweeper(ctx context.Context, every time.Duration, sweep func(time.Time)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			sweep(now)
		}
	}
}

// Stop shuts the listener down gracefully. Safe to call more than once
// and after Start has returned.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil && !errors.Is(err, gosmtp.ErrServerClosed) {
		return err
	}
	log.Printf("%s: stopped", s.name)
	return nil
}

// -----------------------------------------------------------------------
// Backend / Session
// -----------------------------------------------------------------------

// backend builds one session per incoming connection.
type backend struct {
	hostname   string
	store      *store.Store
	spam       *spam.Pipeline
	av         *av.Client
	fwd        ForwarderConfig
	suppressor *vacation.Suppressor
	ledger     *deliveryLedger

	// deliver and enqueue are the store calls that can leave a message
	// half delivered. They are fields so tests can make them fail.
	deliver func(accountID uint64, folder string, in store.IncomingMessage) error
	enqueue func(from string, recipients []string, raw []byte) error
}

func newBackend(hostname string, st *store.Store, sp *spam.Pipeline, avClient *av.Client, fwd ForwarderConfig) *backend {
	return &backend{
		hostname:   hostname,
		store:      st,
		spam:       sp,
		av:         avClient,
		fwd:        fwd,
		suppressor: vacation.NewSuppressor(defaultSuppressWindow),
		ledger:     newDeliveryLedger(),
		deliver: func(accountID uint64, folder string, in store.IncomingMessage) error {
			_, err := st.DeliverTo(accountID, folder, in)
			return err
		},
		enqueue: func(from string, recipients []string, raw []byte) error {
			_, err := st.Enqueue(from, recipients, raw)
			return err
		},
	}
}

// mailboxFull is the temporary refusal for a recipient over quota: the
// sender keeps trying, and the owner may make room in the meantime.
func mailboxFull() *gosmtp.SMTPError {
	return &gosmtp.SMTPError{
		Code:         452,
		EnhancedCode: gosmtp.EnhancedCode{4, 2, 2},
		Message:      "Mailbox full",
	}
}

// sweep expires the backend's in-memory per-sender state.
func (b *backend) sweep(now time.Time) {
	b.suppressor.Sweep(now)
	b.ledger.sweep(now)
}

func (b *backend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &session{backend: b, conn: c, remoteIP: remoteIPOf(c.Conn())}, nil
}

// remoteIPOf returns the IP part of a connection's remote address, or
// "" if the address is missing or unparseable. Used by both SMTP
// backends to key the rate limiter and the spam pipeline.
func remoteIPOf(c net.Conn) string {
	if c == nil {
		return ""
	}
	addr := c.RemoteAddr()
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	return host
}

// session is the per-connection state machine. go-smtp serializes the
// calls for one connection, so the envelope fields need no locking.
type session struct {
	backend  *backend
	conn     *gosmtp.Conn
	remoteIP string

	// Envelope state for the message currently being received; cleared
	// by Reset (which go-smtp calls after each delivered message).
	from       string
	rcptAddrs  []string        // envelope recipients, as given, for the spam pipeline
	rcptAccts  map[uint64]bool // resolved local account IDs, deduplicated across recipients
	rcptRemote []string        // alias forwarding: remote addresses to relay to
	auth       spam.AuthResult // what the spam pipeline learned; set by Data
}

// Mail records the envelope sender (MAIL FROM). An empty sender is the
// null reverse-path used by bounces and is accepted as such. The checks
// that need only the client's IP run here, so a client that is going to
// be turned away is not first allowed to send a 25 MiB body.
func (s *session) Mail(from string, _ *gosmtp.MailOptions) error {
	if v := s.backend.spam.CheckIP(s.remoteIP); v != spam.Accept {
		observability.SMTPMessages.WithLabelValues(verdictLabel(v)).Inc()
		return verdictError(v)
	}
	s.from = from
	return nil
}

// Rcpt resolves an envelope recipient (RCPT TO). Three paths:
//
//  1. The address is an SRS-rewritten bounce target: decode it,
//     dispatch as if the original sender had been written into RCPT,
//     relaying if that sender is remote. This is how alias-forwarding
//     bounces find their way home. Only an address we signed gets
//     here, so this is not an open relay.
//  2. The address is a local mailbox or alias: file copies for the
//     local accounts at DATA, and (for aliases with remote
//     destinations) queue forwards with an SRS-rewritten sender.
//  3. Neither: reject with 550. The inbound MX does not relay open
//     mail.
func (s *session) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	decoded, isSRS := s.decodeIfSRS(to)
	if isSRS {
		to = decoded
	}
	dests, err := s.backend.store.ResolveDestinations(to)
	if err == nil && isSRS && dests.Empty() {
		var hosted bool
		if hosted, err = s.isHostedAddress(to); err == nil && !hosted {
			dests.RemoteAddrs = []string{to}
		}
	}
	if err != nil {
		log.Printf("smtp: resolve recipient %q: %v", to, err)
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message:      "Temporary local problem, please try again later",
		}
	}
	if dests.Empty() {
		return &gosmtp.SMTPError{
			Code:         550,
			EnhancedCode: gosmtp.EnhancedCode{5, 1, 1},
			Message:      "No such user here",
		}
	}
	// An alias points off-server but the operator has not enabled SRS:
	// refuse rather than emit mail without a verifiable envelope sender
	// (it would fail SPF / DMARC at the next hop). A purely-local
	// alias is still allowed.
	if len(dests.RemoteAddrs) > 0 && !s.forwardingEnabled() {
		return &gosmtp.SMTPError{
			Code:         550,
			EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
			Message:      "Alias forwarding to remote addresses is not configured",
		}
	}
	if s.rcptAccts == nil {
		s.rcptAccts = make(map[uint64]bool)
	}
	for _, id := range dests.LocalAccounts {
		s.rcptAccts[id] = true
	}
	for _, addr := range dests.RemoteAddrs {
		if !containsString(s.rcptRemote, addr) {
			s.rcptRemote = append(s.rcptRemote, addr)
		}
	}
	s.rcptAddrs = append(s.rcptAddrs, to)
	return nil
}

// decodeIfSRS turns an SRS-rewritten RCPT TO back into the original
// sender. Returns the original address and true on a successful decode;
// returns (to, false) if the address is not an SRS address or SRS is
// not configured, and logs / returns (to, false) on a tampered or
// expired SRS address (the caller falls back to the usual rejection
// path).
func (s *session) decodeIfSRS(to string) (string, bool) {
	if !s.forwardingEnabled() {
		return to, false
	}
	fwd := s.backend.fwd
	if !srs.Is(to) {
		return to, false
	}
	orig, err := srs.Decode(fwd.SRSSecret, to, fwd.SRSMaxAge)
	if err != nil {
		log.Printf("smtp: decode SRS %q: %v", to, err)
		return to, false
	}
	return orig, true
}

// isHostedAddress reports whether an address that resolved to nothing
// belongs to a domain served here, which makes it a user who does not
// exist rather than a remote address.
func (s *session) isHostedAddress(address string) (bool, error) {
	domain := address[strings.LastIndexByte(address, '@')+1:]
	if strings.EqualFold(domain, s.backend.fwd.ForwarderDomain) {
		return true, nil
	}
	_, err := s.backend.store.GetDomain(domain)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// forwardingEnabled reports whether SRS-based alias forwarding is
// configured on this server.
func (s *session) forwardingEnabled() bool {
	return len(s.backend.fwd.SRSSecret) >= 16 && s.backend.fwd.ForwarderDomain != ""
}

// containsString reports whether s contains v.
func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// verdictError maps a spam verdict to its SMTP rejection, or nil if the
// message may proceed.
func verdictError(v spam.Verdict) error {
	switch v {
	case spam.Greylist:
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 7, 1},
			Message:      "Greylisted, please try again later",
		}
	case spam.RateLimited:
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 7, 1},
			Message:      "Too many messages, please try again later",
		}
	case spam.Reject:
		return &gosmtp.SMTPError{
			Code:         550,
			EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
			Message:      "Message rejected by local policy",
		}
	}
	return nil
}

// Data receives the message body, runs the spam pipeline, and on an
// Accept verdict files the message into every resolved recipient's
// INBOX.
func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message:      "Error reading message data",
		}
	}

	subject, messageID, fromAddr := parseHeaders(raw)

	verdict, auth, err := s.backend.spam.CheckMessage(s.remoteIP, s.from, s.rcptAddrs, raw)
	if err != nil {
		log.Printf("smtp: spam pipeline error: %v", err)
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 7, 0},
			Message:      "Temporary local problem, please try again later",
		}
	}
	s.auth = auth
	observability.SMTPMessages.WithLabelValues(verdictLabel(verdict)).Inc()
	if err := verdictError(verdict); err != nil {
		return err
	}

	// AV scan — sits after the spam pipeline so policy-rejected
	// mail doesn't burn the hash. A signature hit returns 550 with
	// the threat name in the response; the receiving MX learns
	// nothing about our signature DB beyond what's already public
	// (EICAR + any operator-loaded list). nil av.Client is the
	// "scanning disabled" idiom and short-circuits clean.
	if avClient := s.backend.av; avClient != nil {
		v, err := av.ScanMessage(context.Background(), avClient, raw)
		if err != nil {
			log.Printf("smtp: av scan error: %v", err)
			return &gosmtp.SMTPError{
				Code:         451,
				EnhancedCode: gosmtp.EnhancedCode{4, 7, 0},
				Message:      "Temporary local problem, please try again later",
			}
		}
		if !v.OK {
			log.Printf("smtp: rejected message from <%s> — AV signature %q matched", s.from, v.Threat)
			return &gosmtp.SMTPError{
				Code:         550,
				EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
				Message:      "Virus signature detected: " + v.Threat,
			}
		}
	}

	// DMARC p=quarantine: accept the message but file it into Junk
	// rather than INBOX. The sender sees 2xx (the message is not
	// bounced), the recipient finds it on the spam shelf.
	folder := "INBOX"
	if verdict == spam.Quarantine {
		folder = junkFolder
	}

	// Every copy that leaves this function carries the trace and
	// authentication headers; the checks above saw the message as sent.
	stamped := s.stamp(raw)
	in := store.IncomingMessage{
		Raw:       stamped,
		MessageID: messageID,
		Subject:   subject,
		FromAddr:  fromAddr,
	}

	// A sender retrying after a 4xx from here resends to everyone; the
	// ledger skips the accounts that already have this message.
	key := keyOf(s.from, messageID, raw)
	done := s.backend.ledger.delivered(key)
	var kept, filed []uint64 // accounts that keep the message; those filed just now
	var failed int
	var full bool
	for acctID := range s.rcptAccts {
		// Per-account Sieve filter: it can override the chosen folder
		// (fileinto), drop the message (discard), or fall through to
		// the spam-pipeline default. A parse / load error is logged
		// and treated as "no filter" so a botched script never loses
		// mail.
		acctFolder, drop := s.sieveDecide(acctID, in, folder)
		if drop {
			continue
		}
		kept = append(kept, acctID)
		if done[acctID] {
			continue
		}
		if err := s.backend.deliver(acctID, acctFolder, in); err != nil {
			log.Printf("smtp: deliver to account %d (%s) failed: %v", acctID, acctFolder, err)
			failed++
			full = full || errors.Is(err, store.ErrQuotaExceeded)
			continue
		}
		filed = append(filed, acctID)
	}
	if failed > 0 {
		// One DATA command covers every recipient, so we can only return
		// a single status: see deliveryLedger.
		s.backend.ledger.record(key, filed, time.Now())
		if full {
			return mailboxFull()
		}
		return &gosmtp.SMTPError{
			Code:         451,
			EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message:      "Temporary delivery failure, please try again later",
		}
	}

	// Vacation auto-responder: per local recipient with a configured
	// rule, see whether RFC 3834 permits a reply, then build one and
	// drop it on the outbound queue. Errors are best-effort logged —
	// a vacation failure must not bounce the original delivery. An
	// account whose Sieve script discarded the message is not in
	// `kept`: it never saw the mail, so it does not answer it.
	if verdict != spam.Quarantine {
		s.fireVacationReplies(raw, kept)
	}

	// Alias forwarding: relay to the remote alias destinations with an
	// SRS-rewritten envelope sender so SPF / DMARC line up at the next
	// hop. We also try to prepend an ARC instance so the receiving MX
	// can attest to the message's integrity at our hop and inherit
	// the prior authentication state. An empty envelope sender (a
	// bounce) is not rewritten — it stays empty so the receiving MX
	// knows not to bounce it again.
	if len(s.rcptRemote) > 0 {
		sender := s.from
		if sender != "" {
			rewritten, err := srs.Encode(s.backend.fwd.SRSSecret, sender, s.backend.fwd.ForwarderDomain)
			if err != nil {
				log.Printf("smtp: SRS rewrite of %q: %v", sender, err)
				s.backend.ledger.record(key, filed, time.Now())
				return &gosmtp.SMTPError{
					Code:         451,
					EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
					Message:      "Could not rewrite envelope sender for forwarding",
				}
			}
			sender = rewritten
		}
		forwardRaw := s.sealARC(stamped)
		if err := s.backend.enqueue(sender, s.rcptRemote, forwardRaw); err != nil {
			log.Printf("smtp: enqueue forward to %v: %v", s.rcptRemote, err)
			s.backend.ledger.record(key, filed, time.Now())
			return &gosmtp.SMTPError{
				Code:         451,
				EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
				Message:      "Temporary forwarding failure, please try again later",
			}
		}
	}

	s.backend.ledger.forget(key)
	log.Printf("smtp: accepted message from <%s> (%d bytes) — %d local, %d forwarded",
		s.from, len(raw), len(s.rcptAccts), len(s.rcptRemote))
	return nil
}

// stamp returns the message as it goes into mailboxes and the queue,
// with this hop's Received and Authentication-Results fields on top and
// any Authentication-Results forged in our name removed.
func (s *session) stamp(raw []byte) []byte {
	hostname := s.backend.hostname
	var b bytes.Buffer
	b.WriteString(receivedHeader(hostname, s.conn, s.remoteIP, false, time.Now()))
	b.WriteString("Authentication-Results: ")
	b.WriteString(strings.ReplaceAll(s.auth.Value(hostname), "; ", ";\r\n\t"))
	b.WriteString("\r\n")
	b.Write(stripAuthResults(raw, hostname))
	return b.Bytes()
}

// sealARC tries to prepend an ARC instance to the raw message before
// it is forwarded. The signing domain is the forwarder's domain
// (same DKIM key that DKIM signing uses). On any failure the
// original message is returned unchanged — sealing is best-effort,
// never block a delivery for it.
func (s *session) sealARC(raw []byte) []byte {
	if arc.HasChain(raw) {
		// A prior chain already exists. Extending it correctly
		// requires running full RFC 8617 §5.2 chain validation
		// across each prior hop — out of scope for v1 — so we leave
		// the message untouched.
		return raw
	}
	d, err := s.backend.store.GetDomain(s.backend.fwd.ForwarderDomain)
	if err != nil || d.DKIMPrivateKey == "" || d.DKIMSelector == "" {
		return raw // no key configured — silently skip
	}
	key, err := parseRSAKeyPEM(d.DKIMPrivateKey)
	if err != nil {
		log.Printf("smtp: ARC: unparseable DKIM key for %s: %v", s.backend.fwd.ForwarderDomain, err)
		return raw
	}
	sealed, err := arc.Seal(raw, arc.SealOptions{
		Domain:      s.backend.fwd.ForwarderDomain,
		Selector:    d.DKIMSelector,
		Key:         key,
		AuthResults: s.auth.Value(s.backend.fwd.ForwarderDomain),
	})
	if err != nil {
		log.Printf("smtp: ARC seal: %v", err)
		return raw
	}
	return sealed
}

// parseRSAKeyPEM decodes a PEM-encoded PKCS#1 RSA private key — the
// same on-disk format DKIM signing uses.
func parseRSAKeyPEM(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New("smtp: not a PEM block")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// sieveDecide loads the account's Sieve script and runs it against
// the incoming message. The returned folder replaces the spam-
// pipeline default; if drop is true the message is dropped entirely.
// If the account has no script or the script fails to parse, the
// default folder is used and the message is delivered. Quarantine is
// the server's verdict, not a filing preference: a fileinto does not
// move a message out of Junk, though the user may still discard it.
func (s *session) sieveDecide(acctID uint64, in store.IncomingMessage, defaultFolder string) (folder string, drop bool) {
	script, err := s.backend.store.GetSieveScript(acctID)
	if err != nil || script == nil || script.Source == "" {
		return defaultFolder, false
	}
	prog, err := sieve.Parse(script.Source)
	if err != nil {
		log.Printf("smtp: parse sieve script for account %d: %v", acctID, err)
		return defaultFolder, false
	}
	headers, err := sieve.ParseHeaders(in.Raw)
	if err != nil {
		return defaultFolder, false
	}
	actions := prog.Eval(sieve.Message{
		Headers: headers,
		Size:    int64(len(in.Raw)),
		Raw:     in.Raw,
	})
	// Action precedence: an explicit Discard wins; otherwise the
	// last FileInto wins; otherwise the default folder.
	folder = defaultFolder
	for _, a := range actions {
		switch v := a.(type) {
		case sieve.Discard:
			return "", true
		case sieve.FileInto:
			if defaultFolder != junkFolder {
				folder = v.Mailbox
			}
		}
	}
	return folder, false
}

// fireVacationReplies asks the configured vacation rule of each given
// account whether it wants to reply, and queues the replies. Suppressor
// stops re-replying to the same sender within the rule's window.
func (s *session) fireVacationReplies(raw []byte, accounts []uint64) {
	for _, acctID := range accounts {
		v, err := s.backend.store.GetVacation(acctID)
		if err != nil || v == nil || !v.Enabled {
			continue
		}
		acc, err := s.backend.store.GetAccountByID(acctID)
		if err != nil {
			continue
		}
		if !vacation.ShouldReply(raw, s.from, []string{acc.Address}) {
			continue
		}
		suppressor := s.backend.suppressor
		if !suppressor.AllowWithin(acctID, s.from, time.Now(), suppressor.WindowOf(v.SuppressDays)) {
			continue
		}
		messageID := randomMessageID(acc.Address)
		reply, err := vacation.Reply(
			vacation.Rule{Subject: v.Subject, Body: v.Body},
			raw, acc.Address, s.from, messageID, time.Now(),
		)
		if err != nil {
			log.Printf("smtp: vacation reply for account %d: %v", acctID, err)
			continue
		}
		// The reply uses the recipient as its envelope sender (the
		// original sender becomes the recipient). An empty MAIL FROM
		// would also be correct per RFC 3834 — and avoids further
		// loops — but breaks SPF on the destination, so we use the
		// account address.
		if _, err := s.backend.store.Enqueue(acc.Address, []string{s.from}, reply); err != nil {
			log.Printf("smtp: enqueue vacation reply for account %d: %v", acctID, err)
		}
	}
}

// randomMessageID builds a fresh Message-Id for an auto-reply, using
// the account's domain as the right-hand side.
func randomMessageID(accountAddress string) string {
	var buf [12]byte
	_, _ = cryptorand.Read(buf[:])
	domain := "localhost"
	if at := strings.LastIndexByte(accountAddress, '@'); at >= 0 && at < len(accountAddress)-1 {
		domain = accountAddress[at+1:]
	}
	return fmt.Sprintf("%x@%s", buf, domain)
}

// Reset discards the in-progress message's envelope state.
func (s *session) Reset() {
	s.from = ""
	s.rcptAddrs = nil
	s.rcptAccts = nil
	s.rcptRemote = nil
	s.auth = spam.AuthResult{}
}

// Logout releases the session. There is nothing connection-scoped to
// free yet.
func (s *session) Logout() error { return nil }

// verdictLabel maps a spam.Verdict to its Prometheus label value.
func verdictLabel(v spam.Verdict) string {
	switch v {
	case spam.Greylist:
		return "greylist"
	case spam.RateLimited:
		return "ratelimit"
	case spam.Reject:
		return "reject"
	case spam.Quarantine:
		return "quarantine"
	default:
		return "accept"
	}
}

// parseHeaders pulls the metadata fields the store records from the raw
// message. A malformed header block is not fatal — the raw bytes and the
// SMTP envelope are authoritative — so any parse failure yields empty
// strings.
func parseHeaders(raw []byte) (subject, messageID, fromAddr string) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", "", ""
	}
	subject = msg.Header.Get("Subject")
	messageID = strings.Trim(msg.Header.Get("Message-Id"), "<>")
	if addr, err := mail.ParseAddress(msg.Header.Get("From")); err == nil {
		// Keep the (already RFC 2047-decoded) display name on FromAddr
		// when present so the inbox list renders `Alice <addr>`
		// instead of stripping to the bare address. We deliberately
		// do NOT call addr.String(): that would RE-encode a non-ASCII
		// Name back into `=?utf-8?q?...?=`, which is the transport
		// form, not what a human inbox list should show. Empty Name
		// → bare address, the historical shape.
		if addr.Name != "" {
			fromAddr = addr.Name + " <" + addr.Address + ">"
		} else {
			fromAddr = addr.Address
		}
	}
	return subject, messageID, fromAddr
}
