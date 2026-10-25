package im

import (
	"errors"
	"unicode/utf8"
)

func CloneIngressCursor(cursor *IngressCursor) *IngressCursor {
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

func ValidateIngressCursor(platform string, c *IngressCursor) error {
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
		if c.Kind != "wechat" || c.Wechat == nil || c.Telegram != nil || c.Wechat.AccountID == "" || len(c.Wechat.AccountID) > MaxScopedIDBytes || !utf8.ValidString(c.Wechat.AccountID) || c.Wechat.BatchVersion == 0 || len(c.Wechat.Buf) > 64<<10 || !utf8.ValidString(c.Wechat.Buf) {
			return errors.New("invalid Wechat checkpoint")
		}
	default:
		return errors.New("platform does not support a cursor")
	}
	return nil
}
