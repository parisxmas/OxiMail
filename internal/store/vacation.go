package store

import (
	"errors"
	"fmt"
)

// Vacation is one account's auto-responder configuration: when it is
// on, inbound mail that satisfies the RFC 3834 "ought to auto-reply"
// rules gets a canned reply with Subject and Body.
type Vacation struct {
	ID        uint64 `json:"_id,omitempty"`
	AccountID uint64 `json:"account_id"`
	Enabled   bool   `json:"enabled"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	// SuppressDays caps how often we auto-reply to the same sender.
	// Zero means "default" — the consumer chooses, typically 7 days.
	SuppressDays int    `json:"suppress_days,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

// GetVacation returns the auto-responder rule for an account.
// ErrNotFound means "no rule configured" — auto-reply is off.
func (s *Store) GetVacation(accountID uint64) (*Vacation, error) {
	m, err := s.db.FindOne(CollVacations, map[string]any{"account_id": accountID})
	if err != nil {
		return nil, fmt.Errorf("store: get vacation for account %d: %w", accountID, err)
	}
	if m == nil {
		return nil, ErrNotFound
	}
	var v Vacation
	if err := decodeDoc(m, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// SetVacation upserts an account's auto-responder rule.
func (s *Store) SetVacation(accountID uint64, enabled bool, subject, body string, suppressDays int) (*Vacation, error) {
	existing, err := s.GetVacation(accountID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		doc, err := s.db.FindAndModify(
			CollVacations,
			map[string]any{"_id": existing.ID},
			map[string]any{"$set": map[string]any{
				"enabled":       enabled,
				"subject":       subject,
				"body":          body,
				"suppress_days": suppressDays,
				"updated_at":    nowRFC3339(),
			}},
		)
		if err != nil {
			return nil, fmt.Errorf("store: update vacation for account %d: %w", accountID, err)
		}
		if doc == nil {
			return nil, ErrNotFound
		}
		var v Vacation
		if err := decodeDoc(doc, &v); err != nil {
			return nil, err
		}
		return &v, nil
	}
	v := &Vacation{
		AccountID:    accountID,
		Enabled:      enabled,
		Subject:      subject,
		Body:         body,
		SuppressDays: suppressDays,
		UpdatedAt:    nowRFC3339(),
	}
	doc, err := encodeDoc(v)
	if err != nil {
		return nil, err
	}
	resp, err := s.db.Insert(CollVacations, doc)
	if err != nil {
		return nil, fmt.Errorf("store: create vacation for account %d: %w", accountID, err)
	}
	if v.ID, err = insertedID(resp); err != nil {
		return nil, err
	}
	return v, nil
}

// DeleteVacation removes the rule, turning the auto-responder off.
// Deleting an absent rule is not an error.
func (s *Store) DeleteVacation(accountID uint64) error {
	if _, err := s.db.Delete(CollVacations, map[string]any{"account_id": accountID}); err != nil {
		return fmt.Errorf("store: delete vacation for account %d: %w", accountID, err)
	}
	return nil
}
