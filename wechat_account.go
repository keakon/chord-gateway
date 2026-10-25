package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/keakon/chord-gateway/internal/im"
)

func (a *WechatAdapter) prepareIngressAccount(ctx context.Context, token *TokenData) (im.IngressCursor, error) {
	r := a.pairingRouter()
	if token == nil || token.AccountID == "" || a.token.Load() != token || r == nil || r.ingressStore == nil {
		return im.IngressCursor{}, errors.New("wechat ingress account unavailable")
	}
	if a.accountLockID != token.AccountID {
		lock, err := im.AcquireIMAccountLock("wechat", token.AccountID)
		if err != nil {
			return im.IngressCursor{}, fmt.Errorf("lock Wechat account: %w", err)
		}
		a.closeIngressAccount()
		a.accountLock, a.accountLockID = lock, token.AccountID
	}
	if a.token.Load() != token {
		return im.IngressCursor{}, errors.New("wechat account changed during preparation")
	}
	if err := r.PrepareIngressAccount(ctx, "wechat", token.AccountID); err != nil {
		return im.IngressCursor{}, fmt.Errorf("prepare Wechat ingress: %w", err)
	}
	return r.ingressStore.Cursor("wechat", token.AccountID)
}

func (a *WechatAdapter) closeIngressAccount() {
	if a.accountLock != nil {
		a.accountLock.Close()
		a.accountLock = nil
	}
	a.accountLockID = ""
}
