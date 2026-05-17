package store

import (
	"errors"
	"fmt"
)

// SieveScript is one account's stored filter program.
type SieveScript struct {
	ID        uint64 `json:"_id,omitempty"`
	AccountID uint64 `json:"account_id"`
	Source    string `json:"source"`
	UpdatedAt string `json:"updated_at"`
}

// GetSieveScript returns the script bound to an account, or
// ErrNotFound if none is set.
func (s *Store) GetSieveScript(accountID uint64) (*SieveScript, error) {
	m, err := s.db.FindOne(SieveScriptsColl(accountID), map[string]any{"account_id": accountID})
	if err != nil {
		return nil, fmt.Errorf("store: get sieve script for account %d: %w", accountID, err)
	}
	if m == nil {
		return nil, ErrNotFound
	}
	var sc SieveScript
	if err := decodeDoc(m, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// SetSieveScript upserts an account's script.
func (s *Store) SetSieveScript(accountID uint64, source string) (*SieveScript, error) {
	existing, err := s.GetSieveScript(accountID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		doc, err := s.db.FindAndModify(
			SieveScriptsColl(accountID),
			map[string]any{"_id": existing.ID},
			map[string]any{"$set": map[string]any{
				"source":     source,
				"updated_at": nowRFC3339(),
			}},
		)
		if err != nil {
			return nil, fmt.Errorf("store: update sieve script for account %d: %w", accountID, err)
		}
		if doc == nil {
			return nil, ErrNotFound
		}
		var sc SieveScript
		if err := decodeDoc(doc, &sc); err != nil {
			return nil, err
		}
		return &sc, nil
	}
	sc := &SieveScript{AccountID: accountID, Source: source, UpdatedAt: nowRFC3339()}
	doc, err := encodeDoc(sc)
	if err != nil {
		return nil, err
	}
	resp, err := s.db.Insert(SieveScriptsColl(accountID), doc)
	if err != nil {
		return nil, fmt.Errorf("store: create sieve script for account %d: %w", accountID, err)
	}
	if sc.ID, err = insertedID(resp); err != nil {
		return nil, err
	}
	return sc, nil
}

// DeleteSieveScript removes the script.
func (s *Store) DeleteSieveScript(accountID uint64) error {
	if _, err := s.db.Delete(SieveScriptsColl(accountID), map[string]any{"account_id": accountID}); err != nil {
		return fmt.Errorf("store: delete sieve script for account %d: %w", accountID, err)
	}
	return nil
}
