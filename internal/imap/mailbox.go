package imap

import (
	"bytes"
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

	dbID        uint64
	name        string
	uidValidity uint32

	tracker *imapserver.MailboxTracker
	session *imapserver.SessionTracker

	// mu guards msgs and uidNext. The session's own command goroutine
	// and the background refresh goroutine both touch them.
	mu      sync.Mutex
	uidNext uint32
	// msgs is the snapshot, ordered by ascending UID — so the slice
	// index + 1 is the IMAP sequence number.
	msgs []store.Message

	sub  *notifier.Subscription
	done chan struct{}
}

func newSelectedMailbox(st *store.Store, mb *store.Mailbox) (*selectedMailbox, error) {
	msgs, err := st.ListMessages(mb.ID)
	if err != nil {
		return nil, err
	}
	tracker := imapserver.NewMailboxTracker(uint32(len(msgs)))
	m := &selectedMailbox{
		store:       st,
		dbID:        mb.ID,
		name:        mb.Name,
		uidNext:     mb.UIDNext,
		uidValidity: mb.UIDValidity,
		tracker:     tracker,
		session:     tracker.NewSession(),
		msgs:        msgs,
		sub:         notifier.Default.Subscribe(mb.ID),
		done:        make(chan struct{}),
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
//  1. New arrivals (UID >= uidNext, not yet in the snapshot) are
//     appended and an EXISTS is queued.
//  2. Snapshot UIDs missing from the latest list — i.e. expunged by
//     another connection — are removed and EXPUNGE is queued.
//  3. Snapshot messages whose mod-sequence has advanced past our
//     local copy — i.e. another connection ran STORE — get their
//     flags refreshed and FETCH FLAGS (+ MODSEQ) is queued.
//
// QRESYNC nuance: per RFC 7162 §3.7 a QRESYNC-enabled session is
// supposed to see VANISHED instead of EXPUNGE even for cross-
// connection expunges. The upstream tracker only emits EXPUNGE
// today, so QRESYNC clients on this code path still get EXPUNGE for
// cross-connection deletes. Most QRESYNC clients tolerate both, and
// a fully QRESYNC-aware tracker is a separate upstream patch.
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
// consistent snapshot.
func (m *selectedMailbox) refresh() {
	latest, err := m.store.ListMessages(m.dbID)
	if err != nil {
		log.Printf("imap: refresh mailbox %d: %v", m.dbID, err)
		return
	}
	latestByUID := make(map[uint32]*store.Message, len(latest))
	for i := range latest {
		latestByUID[latest[i].UID] = &latest[i]
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// (2) Cross-connection EXPUNGE: snapshot UIDs that no longer
	// exist. Walk back-to-front so the lower indices stay valid as
	// we drop entries.
	for i := len(m.msgs) - 1; i >= 0; i-- {
		if _, ok := latestByUID[m.msgs[i].UID]; ok {
			continue
		}
		m.tracker.QueueExpunge(uint32(i) + 1)
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

	// (1) New arrivals — same logic as before.
	for i := range latest {
		if latest[i].UID < m.uidNext {
			continue
		}
		if containsUID(m.msgs, latest[i].UID) {
			continue
		}
		m.msgs = append(m.msgs, latest[i])
		if latest[i].UID+1 > m.uidNext {
			m.uidNext = latest[i].UID + 1
		}
	}
	m.tracker.QueueNumMessages(uint32(len(m.msgs)))
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
		HighestModSeq:     m.highestModSeqLocked(),
	}
}

// highestModSeqLocked returns the highest mod-sequence across the
// snapshot. The mailbox-level counter is the authoritative number,
// but for the SELECT reply we use the snapshot's max so it matches
// what subsequent FETCH responses will report. The caller must hold
// m.mu.
func (m *selectedMailbox) highestModSeqLocked() uint64 {
	var max uint64
	for i := range m.msgs {
		if m.msgs[i].ModSeq > max {
			max = m.msgs[i].ModSeq
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

	var ferr error
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
		if markSeen && !hasFlag(msg.Flags, string(imap.FlagSeen)) {
			if err := m.store.AddFlags(msg.ID, string(imap.FlagSeen)); err != nil {
				ferr = err
				return
			}
			msg.Flags = append(msg.Flags, string(imap.FlagSeen))
			// AddFlags bumped the message's mod-seq; the local copy
			// has the previous value. Refresh by re-reading from the
			// store so subsequent WriteModSeq emits the new value.
			if fresh, err := m.store.GetMessage(msg.ID); err == nil {
				msg.ModSeq = fresh.ModSeq
			}
			m.tracker.QueueMessageFlags(seqNum, imap.UID(msg.UID), toIMAPFlags(msg.Flags), nil)
		}
		rw := w.CreateMessage(m.session.EncodeSeqNum(seqNum))
		ferr = m.writeMessage(rw, msg, options)
	})
	return ferr
}

// emitVanishedSince writes "* VANISHED (EARLIER) <uids>" for every UID
// in want that has been expunged from this mailbox with mod-sequence
// greater than sinceModSeq. An empty result emits nothing — the spec
// is happy either way. A want set carrying "*" is staticised first so
// the comparison is exact.
func (m *selectedMailbox) emitVanishedSince(w *imapserver.FetchWriter, want imap.UIDSet, sinceModSeq uint64) error {
	uids, err := m.store.ExpungedSince(m.dbID, sinceModSeq)
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

// writeMessage writes one message's requested FETCH items. The raw body
// is read from the blob store only if some requested item needs it.
func (m *selectedMailbox) writeMessage(w *imapserver.FetchResponseWriter, msg *store.Message, options *imap.FetchOptions) error {
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
// When opts.UnchangedSince is non-zero, messages whose mod-sequence
// has advanced past the floor are NOT updated and are returned to the
// client via a MODIFIED response code (RFC 7162 §3.1.3).
func (m *selectedMailbox) storeFlags(w *imapserver.FetchWriter, numSet imap.NumSet, sf *imap.StoreFlags, opts *imap.StoreOptions) error {
	var (
		serr     error
		applied  []uint32 // sequence numbers that did get updated
		modified imap.UIDSet
	)
	m.forEach(numSet, func(seqNum uint32, msg *store.Message) {
		if serr != nil {
			return
		}
		if opts != nil && opts.UnchangedSince != 0 && msg.ModSeq > opts.UnchangedSince {
			// The message has moved on since the client looked; do
			// not apply the change. Report its UID under MODIFIED.
			modified.AddNum(imap.UID(msg.UID))
			return
		}
		newFlags, err := m.applyFlags(msg, sf)
		if err != nil {
			serr = err
			return
		}
		msg.Flags = newFlags
		// applyFlags bumped the message's modseq; refresh the local
		// copy so the post-store FETCH echoes the new value.
		if fresh, err := m.store.GetMessage(msg.ID); err == nil {
			msg.ModSeq = fresh.ModSeq
		}
		m.tracker.QueueMessageFlags(seqNum, imap.UID(msg.UID), toIMAPFlags(newFlags), m.session)
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
			Code: imap.ResponseCode("MODIFIED " + modified.String()),
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

// copy duplicates every message in numSet into dest. Each copy gets a
// fresh UID (via store.NextUID inside CopyMessage); flags carry over.
func (m *selectedMailbox) copy(numSet imap.NumSet, dest *store.Mailbox) (*imap.CopyData, error) {
	var (
		sourceUIDs, destUIDs imap.UIDSet
		copyErr              error
	)
	m.forEach(numSet, func(_ uint32, msg *store.Message) {
		if copyErr != nil {
			return
		}
		copied, err := m.store.CopyMessage(msg.ID, dest.ID)
		if err != nil {
			copyErr = err
			return
		}
		sourceUIDs.AddNum(imap.UID(msg.UID))
		destUIDs.AddNum(imap.UID(copied.UID))
	})
	if copyErr != nil {
		return nil, copyErr
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

	var (
		movedMsgs []moved
		moveErr   error
	)
	m.forEachWithIndex(numSet, func(idx int, msg *store.Message) {
		if moveErr != nil {
			return
		}
		newMsg, err := m.store.MoveMessage(msg.ID, dest.ID)
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
	m.mu.Lock()
	defer m.mu.Unlock()
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

// forEachWithIndex is forEach plus the slice index — move needs it so
// it can drop the right entries from the snapshot.
func (m *selectedMailbox) forEachWithIndex(numSet imap.NumSet, fn func(idx int, msg *store.Message)) {
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
		if err := m.store.DeleteMessage(msg.ID); err != nil {
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
