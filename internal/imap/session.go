package imap

import (
	"errors"
	"io"
	"log"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-sasl"

	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/ratelimit"
	"github.com/parisxmas/OxiMail/internal/store"
)

// session implements imapserver.Session. go-imap serializes the calls
// for one connection, and the framework enforces the IMAP state machine
// (no SELECT before LOGIN, no FETCH before SELECT), so the fields need
// no locking.
type session struct {
	store    *store.Store
	limiter  *ratelimit.Limiter
	remoteIP string
	conn     *imapserver.Conn // for EnabledCaps() (CONDSTORE / QRESYNC checks)

	account *store.Account   // set by Login; nil until authenticated
	mbox    *selectedMailbox // set by Select; nil in the authenticated state
}

// qresyncEnabled reports whether the client has run ENABLE QRESYNC on
// this session. Sessions that have it on get VANISHED responses
// instead of EXPUNGE, and pay attention to the (QRESYNC ...) SELECT
// modifier.
func (s *session) qresyncEnabled() bool {
	if s.conn == nil {
		return false
	}
	return s.conn.EnabledCaps().Has(imap.CapQResync)
}

var (
	_ imapserver.Session     = (*session)(nil)
	_ imapserver.SessionSASL = (*session)(nil)
	_ imapserver.SessionMove = (*session)(nil)
)

// notImplemented is the error returned for commands OxiMail does not
// support yet.
func notImplemented(cmd string) error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeCannot,
		Text: cmd + " is not implemented yet",
	}
}

func noSuchMailbox() error {
	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeNonExistent,
		Text: "No such mailbox",
	}
}

func notSelected() error {
	return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "No mailbox selected"}
}

// normalizeMailbox folds the special name INBOX to its canonical
// spelling; IMAP requires INBOX to be matched case-insensitively.
func normalizeMailbox(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}

// --- Not authenticated state ---

func (s *session) Login(username, password string) error {
	if s.limiter.Blocked(s.remoteIP) {
		observability.Logins.WithLabelValues("imap", "fail").Inc()
		return imapserver.ErrAuthFailed
	}
	acc, err := s.store.Authenticate(username, password)
	if err != nil {
		s.limiter.RecordFailure(s.remoteIP)
		observability.Logins.WithLabelValues("imap", "fail").Inc()
		if errors.Is(err, store.ErrAuthFailed) {
			return imapserver.ErrAuthFailed
		}
		log.Printf("imap: login %q: %v", username, err)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Temporary authentication failure"}
	}
	observability.Logins.WithLabelValues("imap", "ok").Inc()
	s.account = acc
	return nil
}

// AuthenticateMechanisms advertises the SASL mechanisms available
// through the IMAP AUTHENTICATE command.
func (s *session) AuthenticateMechanisms() []string {
	return []string{sasl.Plain}
}

// Authenticate handles SASL authentication. Only PLAIN is offered.
func (s *session) Authenticate(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if s.limiter.Blocked(s.remoteIP) {
			observability.Logins.WithLabelValues("imap", "fail").Inc()
			return imapserver.ErrAuthFailed
		}
		if identity != "" && identity != username {
			s.limiter.RecordFailure(s.remoteIP)
			observability.Logins.WithLabelValues("imap", "fail").Inc()
			return imapserver.ErrAuthFailed
		}
		acc, err := s.store.Authenticate(username, password)
		if err != nil {
			s.limiter.RecordFailure(s.remoteIP)
			observability.Logins.WithLabelValues("imap", "fail").Inc()
			return imapserver.ErrAuthFailed
		}
		observability.Logins.WithLabelValues("imap", "ok").Inc()
		s.account = acc
		return nil
	}), nil
}

// --- Authenticated state ---

func (s *session) Select(name string, opts *imap.SelectOptions) (*imap.SelectData, error) {
	mb, err := s.store.GetMailboxByName(s.account.ID, normalizeMailbox(name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, noSuchMailbox()
	}
	if err != nil {
		return nil, err
	}
	sel, err := newSelectedMailbox(s.store, mb)
	if err != nil {
		return nil, err
	}
	if s.mbox != nil {
		s.mbox.Close()
	}
	s.mbox = sel
	data := sel.selectData()
	// QRESYNC SELECT (RFC 7162 §3.2): when the client passes
	// (QRESYNC <uidvalidity> <modseq> ...) and our UIDValidity
	// matches, populate Vanished with every UID expunged since the
	// client's last known mod-sequence. A UIDValidity mismatch
	// means the client's cache is gone — the spec says we ignore
	// the rest of the QRESYNC payload, which is exactly what
	// happens here (Vanished stays empty, NumMessages and friends
	// are the fresh truth).
	if opts != nil && opts.QResync != nil && opts.QResync.UIDValidity == data.UIDValidity {
		uids, err := s.store.ExpungedSince(mb.ID, opts.QResync.ModSeq)
		if err != nil {
			return nil, err
		}
		// RFC 7162 §3.2: when the client passes a known-uid set the
		// server MAY restrict the VANISHED report to that subset.
		// We honour it when it's non-empty.
		if len(opts.QResync.KnownUIDs) > 0 {
			uids = filterKnownUIDs(uids, opts.QResync.KnownUIDs)
		}
		for _, u := range uids {
			data.Vanished.AddNum(imap.UID(u))
		}
	}
	return data, nil
}

// filterKnownUIDs keeps only the UIDs that are in `known`.
func filterKnownUIDs(uids []uint32, known imap.UIDSet) []uint32 {
	out := uids[:0]
	for _, u := range uids {
		if known.Contains(imap.UID(u)) {
			out = append(out, u)
		}
	}
	return out
}

func (s *session) Unselect() error {
	if s.mbox != nil {
		s.mbox.Close()
		s.mbox = nil
	}
	return nil
}

func (s *session) Create(name string, _ *imap.CreateOptions) error {
	name = strings.TrimRight(normalizeMailbox(name), string(mailboxDelim))
	_, err := s.store.GetMailboxByName(s.account.ID, name)
	if err == nil {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeAlreadyExists,
			Text: "Mailbox already exists",
		}
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err = s.store.CreateMailbox(s.account.ID, name)
	return err
}

func (s *session) Delete(name string) error {
	name = normalizeMailbox(name)
	if name == "INBOX" {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeCannot,
			Text: "INBOX cannot be deleted",
		}
	}
	mb, err := s.store.GetMailboxByName(s.account.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		return noSuchMailbox()
	}
	if err != nil {
		return err
	}
	// Drop the in-memory snapshot if it is the mailbox going away.
	if s.mbox != nil && s.mbox.dbID == mb.ID {
		s.mbox.Close()
		s.mbox = nil
	}
	return s.store.DeleteMailbox(mb.ID)
}

func (s *session) Rename(name, newName string, _ *imap.RenameOptions) error {
	name = normalizeMailbox(name)
	newName = strings.TrimRight(normalizeMailbox(newName), string(mailboxDelim))
	if name == "INBOX" {
		// RFC 3501's "rename INBOX" semantics are weird (move messages,
		// keep INBOX itself, empty). Leave it unsupported for now.
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeCannot,
			Text: "renaming INBOX is not supported",
		}
	}
	mb, err := s.store.GetMailboxByName(s.account.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		return noSuchMailbox()
	}
	if err != nil {
		return err
	}
	if _, err := s.store.GetMailboxByName(s.account.ID, newName); err == nil {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeAlreadyExists,
			Text: "Mailbox already exists",
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return s.store.RenameMailbox(mb.ID, newName)
}

func (s *session) Subscribe(name string) error {
	return s.setSubscribed(name, true)
}

func (s *session) Unsubscribe(name string) error {
	return s.setSubscribed(name, false)
}

func (s *session) setSubscribed(name string, subscribed bool) error {
	mb, err := s.store.GetMailboxByName(s.account.ID, normalizeMailbox(name))
	if errors.Is(err, store.ErrNotFound) {
		return noSuchMailbox()
	}
	if err != nil {
		return err
	}
	return s.store.SetMailboxSubscribed(mb.ID, subscribed)
}

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	// The empty-pattern probe just asks for the hierarchy delimiter.
	if len(patterns) == 0 {
		return w.WriteList(&imap.ListData{
			Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect},
			Delim: mailboxDelim,
		})
	}

	boxes, err := s.store.ListMailboxes(s.account.ID)
	if err != nil {
		return err
	}
	var out []imap.ListData
	for _, mb := range boxes {
		matched := false
		for _, pattern := range patterns {
			if imapserver.MatchList(mb.Name, mailboxDelim, ref, pattern) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if options.SelectSubscribed && !mb.Subscribed {
			continue
		}
		data := imap.ListData{Mailbox: mb.Name, Delim: mailboxDelim}
		if mb.Subscribed {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		out = append(out, data)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mailbox < out[j].Mailbox })
	for i := range out {
		if err := w.WriteList(&out[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	name = normalizeMailbox(name)
	mb, err := s.store.GetMailboxByName(s.account.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, noSuchMailbox()
	}
	if err != nil {
		return nil, err
	}

	// Only enumerate every message when the client asked for an
	// option that needs per-message data (Unseen / Deleted / Size).
	// The common case — a phone polling STATUS MESSAGES / UIDNEXT
	// / HIGHESTMODSEQ once a minute — gets to skip the scan.
	needsList := options.NumUnseen || options.NumDeleted || options.Size
	var msgs []store.Message
	if needsList {
		msgs, err = s.store.ListMessages(mb.ID)
		if err != nil {
			return nil, err
		}
	}

	data := &imap.StatusData{Mailbox: name}
	if options.NumMessages {
		var n uint32
		if needsList {
			n = uint32(len(msgs))
		} else {
			count, err := s.store.CountMessages(mb.ID)
			if err != nil {
				return nil, err
			}
			n = uint32(count)
		}
		data.NumMessages = &n
	}
	if options.UIDNext {
		data.UIDNext = imap.UID(mb.UIDNext)
	}
	if options.UIDValidity {
		data.UIDValidity = mb.UIDValidity
	}
	if options.NumUnseen {
		var n uint32
		for i := range msgs {
			if !hasFlag(msgs[i].Flags, string(imap.FlagSeen)) {
				n++
			}
		}
		data.NumUnseen = &n
	}
	if options.NumDeleted {
		var n uint32
		for i := range msgs {
			if hasFlag(msgs[i].Flags, string(imap.FlagDeleted)) {
				n++
			}
		}
		data.NumDeleted = &n
	}
	if options.Size {
		var sz int64
		for i := range msgs {
			sz += msgs[i].SizeBytes
		}
		data.Size = &sz
	}
	if options.HighestModSeq {
		// The authoritative value is the mailbox-level counter; we
		// take it from the freshly-loaded Mailbox above.
		data.HighestModSeq = mb.HighestModSeq
	}
	return data, nil
}

func (s *session) Append(name string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	mb, err := s.store.GetMailboxByName(s.account.ID, normalizeMailbox(name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	}
	if err != nil {
		return nil, err
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	in := parseIncoming(raw)
	if options != nil {
		in.Flags = flagsToStrings(options.Flags)
		in.InternalDate = options.Time
	}

	msg, err := s.store.AppendMessage(mb.ID, in)
	if err != nil {
		return nil, err
	}

	// If the client has this very mailbox selected, fold the new message
	// into its view so the next poll reports it.
	if s.mbox != nil && s.mbox.dbID == mb.ID {
		s.mbox.add(msg)
	}

	return &imap.AppendData{UIDValidity: mb.UIDValidity, UID: imap.UID(msg.UID)}, nil
}

func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.mbox == nil {
		return nil
	}
	return s.mbox.session.Poll(w, allowExpunge)
}

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if s.mbox == nil {
		<-stop
		return nil
	}
	return s.mbox.session.Idle(w, stop)
}

// --- Selected state ---

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if s.mbox == nil {
		return notSelected()
	}
	return s.mbox.fetch(w, numSet, options)
}

func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, opts *imap.StoreOptions) error {
	if s.mbox == nil {
		return notSelected()
	}
	return s.mbox.storeFlags(w, numSet, flags, opts)
}

func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	if s.mbox == nil {
		return notSelected()
	}
	return s.mbox.expunge(w, uids, s.qresyncEnabled())
}

func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, _ *imap.SearchOptions) (*imap.SearchData, error) {
	if s.mbox == nil {
		return nil, notSelected()
	}
	return s.mbox.search(kind, criteria), nil
}

func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	destMb, err := s.resolveCopyDest(dest)
	if err != nil {
		return nil, err
	}
	return s.mbox.copy(numSet, destMb)
}

// Move implements imapserver.SessionMove — RFC 6851 atomic MOVE. The
// destination gets a fresh document and UID; the source mailbox sees
// EXPUNGE for each moved sequence number, all in one command.
func (s *session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	destMb, err := s.resolveCopyDest(dest)
	if err != nil {
		return err
	}
	return s.mbox.move(w, numSet, destMb)
}

// resolveCopyDest is the shared destination-mailbox lookup for COPY
// and MOVE: same selected-state check, same TRYCREATE response when
// the destination does not exist, same refusal when the destination
// equals the source.
func (s *session) resolveCopyDest(dest string) (*store.Mailbox, error) {
	if s.mbox == nil {
		return nil, notSelected()
	}
	destMb, err := s.store.GetMailboxByName(s.account.ID, normalizeMailbox(dest))
	if errors.Is(err, store.ErrNotFound) {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	}
	if err != nil {
		return nil, err
	}
	if s.mbox.dbID == destMb.ID {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Source and destination mailboxes are identical",
		}
	}
	return destMb, nil
}

// Close releases the session. It is the connection-teardown hook, not
// the IMAP CLOSE command.
func (s *session) Close() error {
	if s.mbox != nil {
		s.mbox.Close()
		s.mbox = nil
	}
	return nil
}
