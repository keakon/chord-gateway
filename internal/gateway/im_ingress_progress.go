package gateway

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/keakon/chord-gateway/internal/im"
)

func (s *IngressStore) checkCursor(platform, account string, expected uint64, next im.IngressCursor) error {
	if account == "" || len(account) > im.MaxScopedIDBytes || !utf8.ValidString(account) {
		return errors.New("invalid cursor account")
	}
	if err := im.ValidateIngressCursor(platform, &next); err != nil {
		return err
	}
	if next.Generation != expected+1 || s.cursors[compositeKey(platform, account)].Generation != expected {
		return im.ErrIngressCursorConflict
	}
	if next.Wechat != nil && next.Wechat.AccountID != account || next.Telegram != nil && next.Telegram.BotID != "" && next.Telegram.BotID != account {
		return errors.New("cursor account mismatch")
	}
	old := s.cursors[compositeKey(platform, account)]
	if old.Wechat != nil && next.Wechat != nil && next.Wechat.BatchVersion <= old.Wechat.BatchVersion {
		return im.ErrIngressCursorConflict
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
		return im.ErrIngressTransportChanged
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := s.checkCursor(platform, account, expected, next); err != nil {
		return err
	}
	s.cursors[compositeKey(platform, account)] = *im.CloneIngressCursor(&next)
	return nil
}
func (s *IngressStore) Cursor(platform, account string) (im.IngressCursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cursors[compositeKey(platform, account)]
	return *im.CloneIngressCursor(&c), nil
}
