package imap

import (
	"bytes"
	"sort"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/parisxmas/OxiMail/internal/store"
)

// selectedMailbox is one session's view of a SELECTed mailbox: a
// snapshot of the message metadata taken at SELECT time, plus the
// go-imap trackers that turn the session's own mutations (STORE,
// EXPUNGE, APPEND) into the right unilateral responses.
//
// Message bodies are not in the snapshot — they are read from the blob
// store lazily, only when a FETCH asks for them.
type selectedMailbox struct {
	store *store.Store

	dbID        uint64
	name        string
	uidNext     uint32
	uidValidity uint32

	tracker *imapserver.MailboxTracker
	session *imapserver.SessionTracker

	// msgs is the snapshot, ordered by ascending UID — so the slice
	// index + 1 is the IMAP sequence number.
	msgs []store.Message
}

func newSelectedMailbox(st *store.Store, mb *store.Mailbox) (*selectedMailbox, error) {
	msgs, err := st.ListMessages(mb.ID)
	if err != nil {
		return nil, err
	}
	tracker := imapserver.NewMailboxTracker(uint32(len(msgs)))
	return &selectedMailbox{
		store:       st,
		dbID:        mb.ID,
		name:        mb.Name,
		uidNext:     mb.UIDNext,
		uidValidity: mb.UIDValidity,
		tracker:     tracker,
		session:     tracker.NewSession(),
		msgs:        msgs,
	}, nil
}

// Close releases the session tracker.
func (m *selectedMailbox) Close() {
	m.session.Close()
}

func (m *selectedMailbox) selectData() *imap.SelectData {
	flags := m.flags()
	permanent := append(append([]imap.Flag(nil), flags...), imap.FlagWildcard)
	return &imap.SelectData{
		Flags:             flags,
		PermanentFlags:    permanent,
		NumMessages:       uint32(len(m.msgs)),
		UIDNext:           imap.UID(m.uidNext),
		UIDValidity:       m.uidValidity,
		FirstUnseenSeqNum: m.firstUnseenSeqNum(),
	}
}

// flags is the set of flags present on the snapshot's messages, plus the
// standard system flags — always advertised so clients can set them.
func (m *selectedMailbox) flags() []imap.Flag {
	set := map[imap.Flag]struct{}{
		imap.FlagSeen:     {},
		imap.FlagAnswered: {},
		imap.FlagFlagged:  {},
		imap.FlagDeleted:  {},
		imap.FlagDraft:    {},
	}
	for i := range m.msgs {
		for _, f := range m.msgs[i].Flags {
			set[imap.Flag(f)] = struct{}{}
		}
	}
	out := make([]imap.Flag, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (m *selectedMailbox) firstUnseenSeqNum() uint32 {
	for i := range m.msgs {
		if !hasFlag(m.msgs[i].Flags, string(imap.FlagSeen)) {
			return uint32(i) + 1
		}
	}
	return 0
}

// add folds a freshly appended message into the snapshot and tells the
// tracker the message count grew.
func (m *selectedMailbox) add(msg *store.Message) {
	m.msgs = append(m.msgs, *msg)
	if msg.UID+1 > m.uidNext {
		m.uidNext = msg.UID + 1
	}
	m.tracker.QueueNumMessages(uint32(len(m.msgs)))
}

// forEach calls fn for every snapshot message in numSet, in sequence
// order. It handles both sequence-number and UID sets and the "*"
// wildcard.
func (m *selectedMailbox) forEach(numSet imap.NumSet, fn func(seqNum uint32, msg *store.Message)) {
	numSet = m.staticNumSet(numSet)
	for i := range m.msgs {
		seqNum := uint32(i) + 1
		var contains bool
		switch ns := numSet.(type) {
		case imap.SeqSet:
			enc := m.session.EncodeSeqNum(seqNum)
			contains = enc != 0 && ns.Contains(enc)
		case imap.UIDSet:
			contains = ns.Contains(imap.UID(m.msgs[i].UID))
		}
		if contains {
			fn(seqNum, &m.msgs[i])
		}
	}
}

// staticNumSet resolves the dynamic "*" marker (the highest sequence
// number, or the highest UID) to a concrete number so Contains works.
func (m *selectedMailbox) staticNumSet(numSet imap.NumSet) imap.NumSet {
	switch ns := numSet.(type) {
	case imap.SeqSet:
		max := uint32(len(m.msgs))
		for i := range ns {
			staticRange(&ns[i].Start, &ns[i].Stop, max)
		}
	case imap.UIDSet:
		max := m.uidNext - 1
		for i := range ns {
			staticRange((*uint32)(&ns[i].Start), (*uint32)(&ns[i].Stop), max)
		}
	}
	return numSet
}

func staticRange(start, stop *uint32, max uint32) {
	dyn := false
	if *start == 0 {
		*start, dyn = max, true
	}
	if *stop == 0 {
		*stop, dyn = max, true
	}
	if dyn && *start > *stop {
		*start, *stop = *stop, *start
	}
}

// fetch writes a FETCH response for every message in numSet.
func (m *selectedMailbox) fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	// A non-peeking BODY[...] fetch implicitly sets \Seen.
	markSeen := false
	for _, bs := range options.BodySection {
		if !bs.Peek {
			markSeen = true
			break
		}
	}

	var ferr error
	m.forEach(numSet, func(seqNum uint32, msg *store.Message) {
		if ferr != nil {
			return
		}
		if markSeen && !hasFlag(msg.Flags, string(imap.FlagSeen)) {
			if err := m.store.AddFlags(msg.ID, string(imap.FlagSeen)); err != nil {
				ferr = err
				return
			}
			msg.Flags = append(msg.Flags, string(imap.FlagSeen))
			m.tracker.QueueMessageFlags(seqNum, imap.UID(msg.UID), toIMAPFlags(msg.Flags), nil)
		}
		rw := w.CreateMessage(m.session.EncodeSeqNum(seqNum))
		ferr = m.writeMessage(rw, msg, options)
	})
	return ferr
}

// writeMessage writes one message's requested FETCH items. The raw body
// is read from the blob store only if some requested item needs it.
func (m *selectedMailbox) writeMessage(w *imapserver.FetchResponseWriter, msg *store.Message, options *imap.FetchOptions) error {
	w.WriteUID(imap.UID(msg.UID))
	if options.Flags {
		w.WriteFlags(toIMAPFlags(msg.Flags))
	}
	if options.InternalDate {
		w.WriteInternalDate(parseTime(msg.InternalDate))
	}
	if options.RFC822Size {
		w.WriteRFC822Size(msg.SizeBytes)
	}

	needBody := options.Envelope ||
		options.BodyStructure != nil ||
		len(options.BodySection) > 0 ||
		len(options.BinarySection) > 0 ||
		len(options.BinarySectionSize) > 0
	if !needBody {
		return w.Close()
	}

	raw, err := m.store.FetchBody(msg)
	if err != nil {
		return err
	}

	if options.Envelope {
		if env := extractEnvelope(raw); env != nil {
			w.WriteEnvelope(env)
		}
	}
	if options.BodyStructure != nil {
		w.WriteBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)))
	}
	for _, bs := range options.BodySection {
		buf := imapserver.ExtractBodySection(bytes.NewReader(raw), bs)
		wc := w.WriteBodySection(bs, int64(len(buf)))
		_, writeErr := wc.Write(buf)
		closeErr := wc.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, bs := range options.BinarySection {
		buf := imapserver.ExtractBinarySection(bytes.NewReader(raw), bs)
		wc := w.WriteBinarySection(bs, int64(len(buf)))
		_, writeErr := wc.Write(buf)
		closeErr := wc.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, bss := range options.BinarySectionSize {
		n := imapserver.ExtractBinarySectionSize(bytes.NewReader(raw), bss)
		w.WriteBinarySectionSize(bss, n)
	}
	return w.Close()
}

// storeFlags applies a STORE flag update to every message in numSet,
// persisting it and queueing the change for the session's next poll.
func (m *selectedMailbox) storeFlags(w *imapserver.FetchWriter, numSet imap.NumSet, sf *imap.StoreFlags) error {
	var serr error
	m.forEach(numSet, func(seqNum uint32, msg *store.Message) {
		if serr != nil {
			return
		}
		newFlags, err := m.applyFlags(msg, sf)
		if err != nil {
			serr = err
			return
		}
		msg.Flags = newFlags
		m.tracker.QueueMessageFlags(seqNum, imap.UID(msg.UID), toIMAPFlags(newFlags), m.session)
	})
	if serr != nil {
		return serr
	}
	// Unless the client asked for a silent STORE, echo the new flags
	// back as a FETCH response.
	if !sf.Silent {
		return m.fetch(w, numSet, &imap.FetchOptions{UID: true, Flags: true})
	}
	return nil
}

// applyFlags persists one message's flag change and returns the
// resulting flag set.
func (m *selectedMailbox) applyFlags(msg *store.Message, sf *imap.StoreFlags) ([]string, error) {
	flags := flagsToStrings(sf.Flags)
	switch sf.Op {
	case imap.StoreFlagsSet:
		if err := m.store.SetFlags(msg.ID, flags); err != nil {
			return nil, err
		}
		return flags, nil
	case imap.StoreFlagsAdd:
		if err := m.store.AddFlags(msg.ID, flags...); err != nil {
			return nil, err
		}
		return unionFlags(msg.Flags, flags), nil
	case imap.StoreFlagsDel:
		if err := m.store.RemoveFlags(msg.ID, flags...); err != nil {
			return nil, err
		}
		return minusFlags(msg.Flags, flags), nil
	default:
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "Unknown STORE operation"}
	}
}

// expunge removes every \Deleted message in scope (all of them, or just
// those in uids for a UID EXPUNGE). The actual EXPUNGE responses are
// flushed to the client by the framework's post-command poll.
func (m *selectedMailbox) expunge(_ *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	// Walk back to front so each removal leaves the lower sequence
	// numbers — and lower slice indices — untouched.
	for i := len(m.msgs) - 1; i >= 0; i-- {
		msg := &m.msgs[i]
		if uids != nil && !uids.Contains(imap.UID(msg.UID)) {
			continue
		}
		if !hasFlag(msg.Flags, string(imap.FlagDeleted)) {
			continue
		}
		if err := m.store.DeleteMessage(msg.ID); err != nil {
			return err
		}
		m.tracker.QueueExpunge(uint32(i) + 1)
		m.msgs = append(m.msgs[:i], m.msgs[i+1:]...)
	}
	return nil
}
