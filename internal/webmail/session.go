package webmail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// sessionTTL is how long a login stays valid before re-authentication.
const sessionTTL = 24 * time.Hour

// session is one authenticated login.
type session struct {
	accountID uint64
	expiresAt time.Time
}

// sessionStore is an in-memory bearer-token store. It is ephemeral: a
// restart logs everyone out, which is acceptable.
//
// TODO: move sessions to OxiMem so they survive a restart and are
// shared across a multi-instance deployment.
type sessionStore struct {
	now func() time.Time // swapped out in tests

	mu       sync.Mutex
	sessions map[string]session
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		now:      time.Now,
		sessions: make(map[string]session),
	}
}

// create issues a fresh, random token for an account.
func (s *sessionStore) create(accountID uint64) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])

	s.mu.Lock()
	s.sessions[token] = session{accountID: accountID, expiresAt: s.now().Add(sessionTTL)}
	s.mu.Unlock()
	return token, nil
}

// delete revokes a token, if it exists. Used by logout to invalidate
// the bearer credential. Deleting an unknown token is a no-op.
func (s *sessionStore) delete(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// lookup resolves a token to its account id, reporting false if the
// token is unknown or expired.
func (s *sessionStore) lookup(token string) (uint64, bool) {
	if token == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[token]
	if !ok {
		return 0, false
	}
	if s.now().After(sess.expiresAt) {
		delete(s.sessions, token)
		return 0, false
	}
	return sess.accountID, true
}

// sweepLoop drops expired sessions on a timer until ctx is cancelled.
func (s *sessionStore) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

func (s *sessionStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for token, sess := range s.sessions {
		if now.After(sess.expiresAt) {
			delete(s.sessions, token)
		}
	}
}
