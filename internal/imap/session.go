package imap

import (
	"errors"
	"io"
	"log"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/parisxmas/OxiMail/internal/observability"
	"github.com/parisxmas/OxiMail/internal/store"
)

// session implements imapserver.Session. go-imap serializes the calls
// for one connection, and the framework enforces the IMAP state machine
// (no SELECT before LOGIN, no FETCH before SELECT), so the fields need
// no locking.
type session struct {
	store *store.Store

	account *store.Account   // set by Login; nil until authenticated
	mbox    *selectedMailbox // set by Select; nil in the authenticated state
}

var _ imapserver.Session = (*session)(nil)

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
	acc, err := s.store.Authenticate(username, password)
	if err != nil {
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

// --- Authenticated state ---

func (s *session) Select(name string, _ *imap.SelectOptions) (*imap.SelectData, error) {
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
	return sel.selectData(), nil
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

func (s *session) Delete(string) error {
	// TODO: needs a store.DeleteMailbox that cascades messages + blobs.
	return notImplemented("DELETE")
}

func (s *session) Rename(string, string, *imap.RenameOptions) error {
	// TODO: needs a store.RenameMailbox.
	return notImplemented("RENAME")
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
	msgs, err := s.store.ListMessages(mb.ID)
	if err != nil {
		return nil, err
	}

	data := &imap.StatusData{Mailbox: name}
	if options.NumMessages {
		n := uint32(len(msgs))
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

func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, _ *imap.StoreOptions) error {
	if s.mbox == nil {
		return notSelected()
	}
	return s.mbox.storeFlags(w, numSet, flags)
}

func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	if s.mbox == nil {
		return notSelected()
	}
	return s.mbox.expunge(w, uids)
}

func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, _ *imap.SearchOptions) (*imap.SearchData, error) {
	if s.mbox == nil {
		return nil, notSelected()
	}
	return s.mbox.search(kind, criteria), nil
}

func (s *session) Copy(imap.NumSet, string) (*imap.CopyData, error) {
	// TODO: copy messages (metadata + body blob) into another mailbox.
	return nil, notImplemented("COPY")
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
