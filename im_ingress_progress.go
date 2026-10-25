package main

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/internal/im"
)

var errIngressCursorConflict = errors.New("ingress cursor generation conflict")

func cloneIngressCursor(cursor *im.IngressCursor) *im.IngressCursor {
	if cursor == nil {
		return nil
	}
	copy := *cursor
	if cursor.Telegram != nil {
		value := *cursor.Telegram
		if value.PendingOffset != nil {
			offset := *value.PendingOffset
			value.PendingOffset = &offset
		}
		if value.LastObservedUpdateID != nil {
			id := *value.LastObservedUpdateID
			value.LastObservedUpdateID = &id
		}
		copy.Telegram = &value
	}
	if cursor.Wechat != nil {
		value := *cursor.Wechat
		copy.Wechat = &value
	}
	return &copy
}

func validateIngressCursor(platform string, c *im.IngressCursor) error {
	if c == nil {
		return nil
	}
	if c.Version != 1 || c.Generation == 0 {
		return errors.New("invalid cursor version or generation")
	}
	switch platform {
	case "telegram":
		if c.Kind != "telegram" || c.Telegram == nil || c.Wechat != nil {
			return errors.New("invalid Telegram cursor kind")
		}
		t := c.Telegram
		if t.State != "pending" && t.State != "confirmed" && t.State != "empty" {
			return errors.New("invalid Telegram checkpoint state")
		}
		if t.State == "pending" {
			if t.PendingOffset == nil || *t.PendingOffset < 0 || t.ObservedAt.IsZero() {
				return errors.New("invalid Telegram pending offset")
			}
		} else if t.PendingOffset != nil {
			return errors.New("nonpending Telegram checkpoint contains offset")
		}
	case "wechat":
		if c.Kind != "wechat" || c.Wechat == nil || c.Telegram != nil || c.Wechat.AccountID == "" || len(c.Wechat.AccountID) > im.MaxScopedIDBytes || !utf8.ValidString(c.Wechat.AccountID) || c.Wechat.BatchVersion == 0 || len(c.Wechat.Buf) > 64<<10 || !utf8.ValidString(c.Wechat.Buf) {
			return errors.New("invalid Wechat checkpoint")
		}
	default:
		return errors.New("platform does not support a cursor")
	}
	return nil
}
func (s *IngressStore) checkCursor(platform, account string, expected uint64, next im.IngressCursor) error {
	if account == "" || len(account) > im.MaxScopedIDBytes || !utf8.ValidString(account) {
		return errors.New("invalid cursor account")
	}
	if err := validateIngressCursor(platform, &next); err != nil {
		return err
	}
	if next.Generation != expected+1 || s.cursors[compositeKey(platform, account)].Generation != expected {
		return errIngressCursorConflict
	}
	if next.Wechat != nil && next.Wechat.AccountID != account || next.Telegram != nil && next.Telegram.BotID != "" && next.Telegram.BotID != account {
		return errors.New("cursor account mismatch")
	}
	old := s.cursors[compositeKey(platform, account)]
	if old.Wechat != nil && next.Wechat != nil && next.Wechat.BatchVersion <= old.Wechat.BatchVersion {
		return errIngressCursorConflict
	}
	return nil
}
func (s *IngressStore) CommitCursor(ctx context.Context, platform, account string, expected uint64, next im.IngressCursor) error {
	return s.commitCursorGuarded(ctx, platform, account, expected, next, nil)
}
func (s *IngressStore) commitCursorGuarded(ctx context.Context, platform, account string, expected uint64, next im.IngressCursor, valid func() bool) error {
	// Check the adapter callback before taking the store mutex: it reads an
	// adapter lock, and one cursor commit must not serialize behind it.
	if valid != nil && !valid() {
		return errIngressTransportChanged
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := s.checkCursor(platform, account, expected, next); err != nil {
		return err
	}
	s.cursors[compositeKey(platform, account)] = *cloneIngressCursor(&next)
	return nil
}
func (s *IngressStore) Cursor(platform, account string) (im.IngressCursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cursors[compositeKey(platform, account)]
	return *cloneIngressCursor(&c), nil
}
