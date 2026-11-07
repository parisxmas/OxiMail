package imap

import (
	"bytes"
	"errors"
	"log"
	"sort"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/parisxmas/OxiMail/internal/notifier"
	"github.com/parisxmas/OxiMail/internal/store"
)

// selectedMailbox is one session's view of a SELECTed mailbox: a
// snapshot of the message metadata taken at SELECT time, plus the
// go-imap trackers that turn the session's own mutations (STORE,
// EXPUNGE, APPEND) into the right unilateral responses, plus a
// notifier subscription that turns *other* connections' arrivals into
// an unsolicited EXISTS — which is what makes IDLE useful.
//
// Message bodies are not in the snapshot — they are read from the blob
// store lazily, only when a FETCH asks for them.
type selectedMailbox struct {
	store *store.Store

	accountID   uint64 // owner; required to route every per-account store call
	dbID        uint64
	name        string
	uidValidity uint32

	tracker *imapserver.MailboxTracker
	session *imapserver.SessionTracker

	// highestModSeq is what SELECT reports, fixed when the snapshot is
	// taken.
	highestModSeq uint64

	// mu guards msgs and uidNext. The session's own command goroutine
	// and the background refresh goroutine both touch them.
	mu      sync.Mutex
	uidNext uint32
	// msgs is the snapshot; the slice index + 1 is the IMAP sequence
	// number. It is ordered by ascending UID, except for the rare late
	// arrival refresh appends (see there).
	msgs []store.Message

	sub  *notifier.Subscription
	done chan struct{}
}

func newSelectedMailbox(st *store.Store, mb *store.Mailbox) (*selectedMailbox, error) {
	msgs, err := st.ListMessages(mb.AccountID, mb.ID)
	if err != nil {
		return nil, err
	}
	tracker := imapserver.NewMailboxTracker(uint32(len(msgs)))
	m := &selectedMailbox{
		store:         st,
		accountID:     mb.AccountID,
		dbID:          mb.ID,
		name:          mb.Name,
		uidNext:       mb.UIDNext,
		uidValidity:   mb.UIDValidity,
		highestModSeq: highestModSeq(mb, msgs),
		tracker:       tracker,
		session:       tracker.NewSession(),
		msgs:          msgs,
		sub:           notifier.Default.Subscribe(mb.ID),
		done:          make(chan struct{}),
	}
	go m.watch()
	return m, nil
}

// Close releases the session tracker and unsubscribes from the hub.
func (m *selectedMailbox) Close() {
	close(m.done)
	m.sub.Close()
	m.session.Close()
}

// watch wakes on every cross-connection mailbox change, re-lists the
// mailbox, and reconciles the snapshot — handling three deltas so an
// IDLE'ing client sees the full picture:
//
//  1. New arrivals (any UID not yet in the snapshot) are appended and
//     an EXISTS is queued.
//  2. Snapshot UIDs missing from the latest list — i.e. expunged by
//     another connection — are removed and EXPUNGE is queued.
//  3. Snapshot messages whose mod-sequence has advanced past our
//     local copy — i.e. another connection ran STORE — get their
//     flags refreshed and FETCH FLAGS (+ MODSEQ) is queued.
//
// Cross-connection expunges go through MailboxTracker.
// QueueExpungeWithUID, which lets the framework emit "* VANISHED
// <uid>" for QRESYNC-enabled sessions (RFC 7162 §3.7) and plain
// EXPUNGE for everyone else.
func (m *selectedMailbox) watch() {
	for {
		select {
		case <-m.done:
			return
		case <-m.sub.C():
			m.refresh()
		}
	}
}

// refresh re-lists the mailbox and applies the three deltas described
// on watch(). Held under m.mu so any session command runs against a
// consistent snapshot. The list itself happens under the lock too: a
// list taken before one of this session's own EXPUNGE or MOVE commands
// would otherwise bring the removed message back as a new arrival.
func (m *selectedMailbox) refresh() {
	m.mu.Lock()
	defer m.mu.Unlock()

	latest, err := m.store.ListMessages(m.accountID, m.dbID)
	if err != nil {
		log.Printf("imap: refresh mailbox %d: %v", m.dbID, err)
		return
	}
	latestByUID := make(map[uint32]*store.Message, len(latest))
	for i := range latest {
		latestByUID[latest[i].UID] = &latest[i]
	}

	// (2) Cross-connection EXPUNGE: snapshot UIDs that no longer
	// exist. Walk back-to-front so the lower indices stay valid as
	// we drop entries. Queue with the UID so a QRESYNC-enabled
	// session gets the VANISHED form.
	for i := len(m.msgs) - 1; i >= 0; i-- {
		uid := m.msgs[i].UID
		if _, ok := latestByUID[uid]; ok {
			continue
		}
		m.tracker.QueueExpungeWithUID(uint32(i)+1, imap.UID(uid))
		m.msgs = append(m.msgs[:i], m.msgs[i+1:]...)
	}

	// (3) Cross-connection STORE: surviving snapshot messages whose
	// mod-sequence has moved past our local copy. We pass nil for
	// the source session so this session sees the unilateral FETCH
	// FLAGS (the change wasn't made here).
	for i := range m.msgs {
		local := &m.msgs[i]
		fresh, ok := latestByUID[local.UID]
		if !ok || fresh.ModSeq <= local.ModSeq {
			continue
		}
		local.Flags = append(local.Flags[:0], fresh.Flags...)
		local.ModSeq = fresh.ModSeq
		seqNum := uint32(i) + 1
		m.tracker.QueueMessageFlags(seqNum, imap.UID(local.UID), toIMAPFlags(local.Flags), nil)
	}

	// (1) New arrivals. A UID below uidNext can still be new: the store
	// orders its own writers (see store.assignUID) but not another
	// process, whose lower UID can land after a higher one was seen.
	// It goes on the end, out of UID order for the rest of this session,
	// because a slot in the middle would silently renumber every later
	// message under a client that has already seen their sequence
	// numbers. The next SELECT sorts it back into place.
	known := make(map[uint32]bool, len(m.msgs))
	for i := range m.msgs {
		known[m.msgs[i].UID] = true
	}
	added := false
	for i := range latest {
		if known[latest[i].UID] {
			continue
		}
		m.msgs = append(m.msgs, latest[i])
		if latest[i].UID+1 > m.uidNext {
			m.uidNext = latest[i].UID + 1
		}
		added = true
	}
	// Only re-announce the count when something was actually
	// appended. QueueExpunge already decremented the tracker count
	// internally, so an expunge-only refresh leaves the tracker in
	// the right state; calling QueueNumMessages with the post-
	// expunge count would (a) be redundant and (b) panic upstream
	// when that count is zero (the dispatch switch has no case for
	// a no-op update).
	if added {
		m.tracker.QueueNumMessages(uint32(len(m.msgs)))
	}
}

// containsUID reports whether any message in msgs has uid.
func containsUID(msgs []store.Message, uid uint32) bool {
	for i := range msgs {
		if msgs[i].UID == uid {
			return true
		}
	}
	return false
}

func (m *selectedMailbox) selectData() *imap.SelectData {
	m.mu.Lock()
	defer m.mu.Unlock()
	flags := m.flagsLocked()
	permanent := append(append([]imap.Flag(nil), flags...), imap.FlagWildcard)
	return &imap.SelectData{
		Flags:             flags,
		PermanentFlags:    permanent,
		NumMessages:       uint32(len(m.msgs)),
		UIDNext:           imap.UID(m.uidNext),
		UIDValidity:       m.uidValidity,
		FirstUnseenSeqNum: m.firstUnseenSeqNumLocked(),
		HighestModSeq:     m.highestModSeq,
	}
}

// highestModSeq returns the mailbox's HIGHESTMODSEQ. The mailbox-level
// counter is the authoritative number: unlike the snapshot's max it
// counts expunges, so it cannot go down between SELECTs (RFC 7162
// §3.1.2.1). The snapshot is listed after the counter is read and may
// be slightly ahead of it, hence the max.
func highestModSeq(mb *store.Mailbox, msgs []store.Message) uint64 {
	max := mb.HighestModSeq
	for i := range msgs {
		if msgs[i].ModSeq > max {
			max = msgs[i].ModSeq
		}
	}
	return max
}

// flagsLocked is the set of flags present on the snapshot's messages,
// plus the standard system flags — always advertised so clients can
// set them. The caller must hold m.mu.
func (m *selectedMailbox) flagsLocked() []imap.Flag {
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

// firstUnseenSeqNumLocked returns the sequence number of the first
// \Unseen message, or 0 if there is none. The caller must hold m.mu.
func (m *selectedMailbox) firstUnseenSeqNumLocked() uint32 {
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if containsUID(m.msgs, msg.UID) {
		return // already folded in by a notifier refresh
	}
	m.msgs = append(m.msgs, *msg)
	if msg.UID+1 > m.uidNext {
		m.uidNext = msg.UID + 1
	}
	m.tracker.QueueNumMessages(uint32(len(m.msgs)))
}

// snapshot returns a shallow copy of the message slice. Callers iterate
// the copy without holding m.mu so that streaming work (FETCH bodies)
// does not block notifier refreshes.
func (m *selectedMailbox) snapshot() []store.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Message, len(m.msgs))
	copy(out, m.msgs)
	return out
}

// forEach calls fn for every snapshot message in numSet, in sequence
// order. It handles both sequence-number and UID sets and the "*"
// wildcard. m.mu is held for the entire walk so the watch goroutine
// cannot grow the slice underneath; if the callback does external I/O
// (e.g. fetching a body) that I/O happens with the lock held.
func (m *selectedMailbox) forEach(numSet imap.NumSet, fn func(seqNum uint32, msg *store.Message)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	numSet = m.staticNumSetLocked(numSet)
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

// staticNumSetLocked resolves the dynamic "*" marker (the highest
// sequence number, or the highest UID) to a concrete number so
// Contains works. The caller must hold m.mu.
func (m *selectedMailbox) staticNumSetLocked(numSet imap.NumSet) imap.NumSet {
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

	// RFC 7162 §3.2.10: UID FETCH ... (CHANGEDSINCE N VANISHED) asks
	// the server to emit one VANISHED (EARLIER) line listing every
	// UID in numSet that has been expunged since the floor. The
	// per-message FETCH responses follow as normal. The framework's
	// fetch.go has already validated that VANISHED only arrives on
	// UID FETCH with a non-zero CHANGEDSINCE.
	if options.Vanished {
		uidSet, _ := numSet.(imap.UIDSet)
		if err := m.emitVanishedSince(w, uidSet, options.ChangedSince); err != nil {
			return err
		}
	}

	needBody := options.Envelope ||
		options.BodyStructure != nil ||
		len(options.BodySection) > 0 ||
		len(options.BinarySection) > 0 ||
		len(options.BinarySectionSize) > 0

	var ferr, bodyErr error
	m.forEach(numSet, func(seqNum uint32, msg *store.Message) {
		if ferr != nil {
			return
		}
		// RFC 7162 §3.2: CHANGEDSINCE skips messages whose mod-seq
		// is at or below the floor. Mod-seq zero on the message
		// (legacy data from before CONDSTORE was enabled) is
		// treated as "older than anything" and is filtered out.
		if options.ChangedSince != 0 && msg.ModSeq <= options.ChangedSince {
			return
		}
		// The body must be in hand before CreateMessage: from that call
		// until FetchResponseWriter.Close the connection's encoder mutex
		// is held, and the tagged reply needs it. It also keeps \Seen
		// off a message the client never got to read. An unreadable body
		// fails this message only; the rest of the set is still served
		// and the command ends NO.
		var raw []byte
		if needBody {
			var err error
			if raw, err = m.store.FetchBody(msg); err != nil {
				log.Printf("imap: fetch mailbox %d UID %d: %v", m.dbID, msg.UID, err)
				bodyErr = &imap.Error{
					Type: imap.StatusResponseTypeNo,
					Code: imap.ResponseCodeUnavailable,
					Text: "Some message bodies could not be read",
				}
				return
			}
		}
		if markSeen && !hasFlag(msg.Flags, string(imap.FlagSeen)) {
			updated, err := m.store.UpdateFlags(m.accountID, msg.ID, 0, func(cur []string) []string {
				return unionFlags(cur, []string{string(imap.FlagSeen)})
			})
			if err != nil {
				ferr = err
				return
			}
			msg.Flags, msg.ModSeq = updated.Flags, updated.ModSeq
			m.tracker.QueueMessageFlags(seqNum, imap.UID(msg.UID), toIMAPFlags(msg.Flags), nil)
		}
		rw := w.CreateMessage(m.session.EncodeSeqNum(seqNum))
		ferr = m.writeMessage(rw, msg, raw, options)
	})
	if ferr != nil {
		return ferr
	}
	return bodyErr
}

// emitVanishedSince writes "* VANISHED (EARLIER) <uids>" for every UID
// in want that has been expunged from this mailbox with mod-sequence
// greater than sinceModSeq. An empty result emits nothing — the spec
// is happy either way. A want set carrying "*" is staticised first so
// the comparison is exact.
func (m *selectedMailbox) emitVanishedSince(w *imapserver.FetchWriter, want imap.UIDSet, sinceModSeq uint64) error {
	uids, err := m.store.ExpungedSince(m.accountID, m.dbID, sinceModSeq)
	if err != nil {
		return err
	}
	if len(uids) == 0 {
		return nil
	}
	m.mu.Lock()
	staticWant, _ := m.staticNumSetLocked(want).(imap.UIDSet)
	m.mu.Unlock()
	var matched imap.UIDSet
	for _, u := range uids {
		uid := imap.UID(u)
		// An empty/absent want acts as "the whole UID space" — the
		// most common form is "UID FETCH 1:* ...". An explicit set
		// is honoured.
		if len(staticWant) == 0 || staticWant.Contains(uid) {
			matched.AddNum(uid)
		}
	}
	if len(matched) == 0 {
		return nil
	}
	return w.WriteVanished(matched)
}

// writeMessage writes one message's requested FETCH items and closes w.
// It closes w on every path: an unclosed FetchResponseWriter keeps the
// connection's encoder mutex locked, and the session then hangs waiting
// to send the tagged reply. raw is the body, nil if no item needs it.
func (m *selectedMailbox) writeMessage(w *imapserver.FetchResponseWriter, msg *store.Message, raw []byte, options *imap.FetchOptions) error {
	err := m.writeItems(w, msg, raw, options)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

func (m *selectedMailbox) writeItems(w *imapserver.FetchResponseWriter, msg *store.Message, raw []byte, options *imap.FetchOptions) error {
	w.WriteUID(imap.UID(msg.UID))
	if options.Flags {
		w.WriteFlags(toIMAPFlags(msg.Flags))
	}
	if options.ModSeq {
		w.WriteModSeq(msg.ModSeq)
	}
	if options.InternalDate {
		w.WriteInternalDate(parseTime(msg.InternalDate))
	}
	if options.RFC822Size {
		w.WriteRFC822Size(msg.SizeBytes)
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
	return nil
}

// storeFlags applies a STORE flag update to every message in numSet,
// persisting it and queueing the change for the session's next poll.
// When opts.UnchangedSince is non-zero, messages whose stored
// mod-sequence has advanced past the floor are NOT updated and are
// returned to the client via a MODIFIED response code (RFC 7162
// §3.1.3). The comparison is the store's, not the snapshot's, which can
// lag behind another connection's STORE.
func (m *selectedMailbox) storeFlags(w *imapserver.FetchWriter, numSet imap.NumSet, sf *imap.StoreFlags, opts *imap.StoreOptions) error {
	var (
		serr     error
		applied  []uint32 // sequence numbers that did get updated
		modified imap.UIDSet
	)
	var unchangedSince uint64
	if opts != nil {
		unchangedSince = opts.UnchangedSince
	}
	m.forEach(numSet, func(seqNum uint32, msg *store.Message) {
		if serr != nil {
			return
		}
		updated, err := m.applyFlags(msg, sf, unchangedSince)
		if errors.Is(err, store.ErrModified) {
			// The message has moved on since the client looked; the
			// change was not applied. Report its UID under MODIFIED.
			modified.AddNum(imap.UID(msg.UID))
			return
		}
		if err != nil {
			serr = err
			return
		}
		// Take flags and modseq as stored, so the post-store FETCH
		// echoes them and a concurrent change is not masked.
		msg.Flags, msg.ModSeq = updated.Flags, updated.ModSeq
		m.tracker.QueueMessageFlags(seqNum, imap.UID(msg.UID), toIMAPFlags(msg.Flags), m.session)
		applied = append(applied, seqNum)
	})
	if serr != nil {
		return serr
	}
	// Echo the new flags (and the new MODSEQ when CONDSTORE is in
	// play) for the messages we actually updated, unless the client
	// asked for a silent STORE.
	if !sf.Silent && len(applied) > 0 {
		fetchOpts := &imap.FetchOptions{UID: true, Flags: true}
		if opts != nil && opts.UnchangedSince != 0 {
			fetchOpts.ModSeq = true
		}
		if err := m.fetch(w, appliedToNumSet(applied), fetchOpts); err != nil {
			return err
		}
	}
	if len(modified) > 0 {
		return &imap.Error{
			Type: imap.StatusResponseTypeOK,
			Code: imap.ResponseCode(string(imap.ResponseCodeModified) + " " + modified.String()),
			Text: "Some messages have changed since UNCHANGEDSINCE",
		}
	}
	return nil
}

// appliedToNumSet converts a sequence-number list to an imap.SeqSet.
func appliedToNumSet(seqs []uint32) imap.NumSet {
	var ss imap.SeqSet
	for _, n := range seqs {
		ss.AddNum(n)
	}
	return ss
}

// applyFlags persists one message's flag change and returns the message
// as stored.
func (m *selectedMailbox) applyFlags(msg *store.Message, sf *imap.StoreFlags, unchangedSince uint64) (*store.Message, error) {
	flags := flagsToStrings(sf.Flags)
	var mutate func(cur []string) []string
	switch sf.Op {
	case imap.StoreFlagsSet:
		mutate = func([]string) []string { return flags }
	case imap.StoreFlagsAdd:
		mutate = func(cur []string) []string { return unionFlags(cur, flags) }
	case imap.StoreFlagsDel:
		mutate = func(cur []string) []string { return minusFlags(cur, flags) }
	default:
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "Unknown STORE operation"}
	}
	return m.store.UpdateFlags(m.accountID, msg.ID, unchangedSince, mutate)
}

// copy duplicates every message in numSet into dest. Each copy gets a
// fresh UID (via store.NextUID inside CopyMessage); flags carry over.
// The quota is checked for the whole set first, because a COPY that
// fails must not leave part of the set in the destination.
func (m *selectedMailbox) copy(numSet imap.NumSet, dest *store.Mailbox) (*imap.CopyData, error) {
	var total int64
	m.forEach(numSet, func(_ uint32, msg *store.Message) {
		total += msg.SizeBytes
	})
	if err := m.store.CheckQuota(m.accountID, total); err != nil {
		return nil, storeError(err)
	}

	var (
		sourceUIDs, destUIDs imap.UIDSet
		copyErr              error
	)
	m.forEach(numSet, func(_ uint32, msg *store.Message) {
		if copyErr != nil {
			return
		}
		copied, err := m.store.CopyMessage(m.accountID, msg.ID, dest.ID)
		if err != nil {
			copyErr = err
			return
		}
		sourceUIDs.AddNum(imap.UID(msg.UID))
		destUIDs.AddNum(imap.UID(copied.UID))
	})
	if copyErr != nil {
		return nil, storeError(copyErr)
	}
	return &imap.CopyData{
		UIDValidity: dest.UIDValidity,
		SourceUIDs:  sourceUIDs,
		DestUIDs:    destUIDs,
	}, nil
}

// move relocates every message in numSet into dest atomically (per
// RFC 6851): the body blob stays put, the message document gets a new
// mailbox_id and a fresh UID, and the snapshot drops the original
// entry — so the client sees one COPYUID response followed by one
// EXPUNGE per moved message.
func (m *selectedMailbox) move(w *imapserver.MoveWriter, numSet imap.NumSet, dest *store.Mailbox) error {
	type moved struct {
		index   int // position in m.msgs at the time we acted on it
		srcUID  uint32
		destUID uint32
	}

	// m.mu is held from the first store move to the last snapshot
	// removal: a refresh in between would drop the moved messages itself
	// and shift the indices recorded below.
	m.mu.Lock()
	defer m.mu.Unlock()

	var (
		movedMsgs []moved
		moveErr   error
	)
	m.forEachWithIndexLocked(numSet, func(idx int, msg *store.Message) {
		if moveErr != nil {
			return
		}
		newMsg, err := m.store.MoveMessage(m.accountID, msg.ID, dest.ID)
		if err != nil {
			moveErr = err
			return
		}
		movedMsgs = append(movedMsgs, moved{index: idx, srcUID: msg.UID, destUID: newMsg.UID})
	})
	if moveErr != nil {
		return moveErr
	}

	var sourceUIDs, destUIDs imap.UIDSet
	for _, mv := range movedMsgs {
		sourceUIDs.AddNum(imap.UID(mv.srcUID))
		destUIDs.AddNum(imap.UID(mv.destUID))
	}
	if err := w.WriteCopyData(&imap.CopyData{
		UIDValidity: dest.UIDValidity,
		SourceUIDs:  sourceUIDs,
		DestUIDs:    destUIDs,
	}); err != nil {
		return err
	}

	// Remove the moved messages from the snapshot, back-to-front so
	// the higher sequence numbers stay valid as we drop the lower
	// ones. Then emit one EXPUNGE per removed message.
	for i := len(movedMsgs) - 1; i >= 0; i-- {
		idx := movedMsgs[i].index
		if idx < 0 || idx >= len(m.msgs) {
			continue // snapshot shifted underneath us; skip rather than panic
		}
		seqNum := uint32(idx) + 1
		m.tracker.QueueExpunge(seqNum)
		m.msgs = append(m.msgs[:idx], m.msgs[idx+1:]...)
		if err := w.WriteExpunge(seqNum); err != nil {
			return err
		}
	}
	return nil
}

// forEachWithIndexLocked is forEach plus the slice index — move needs
// it so it can drop the right entries from the snapshot. The caller
// must hold m.mu.
func (m *selectedMailbox) forEachWithIndexLocked(numSet imap.NumSet, fn func(idx int, msg *store.Message)) {
	numSet = m.staticNumSetLocked(numSet)
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
			fn(i, &m.msgs[i])
		}
	}
}

// expunge removes every \Deleted message in scope (all of them, or
// just those in uids for a UID EXPUNGE). When qresync is true, the
// per-message EXPUNGE responses are replaced by a single
// "* VANISHED <uids>" line written directly via the ExpungeWriter
// (RFC 7162 §3.2). Otherwise the actual EXPUNGE responses are flushed
// to the client by the framework's post-command poll.
func (m *selectedMailbox) expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet, qresync bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var vanished imap.UIDSet
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
		if err := m.store.DeleteMessage(m.accountID, msg.ID); err != nil {
			return err
		}
		if qresync {
			vanished.AddNum(imap.UID(msg.UID))
		} else {
			m.tracker.QueueExpunge(uint32(i) + 1)
		}
		m.msgs = append(m.msgs[:i], m.msgs[i+1:]...)
	}
	if qresync && len(vanished) > 0 {
		return w.WriteVanished(vanished)
	}
	return nil
}
