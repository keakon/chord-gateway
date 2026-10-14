package main

import (
	"context"
	"errors"
	"fmt"
)

func (a *WechatAdapter) prepareIngressAccount(ctx context.Context, token *TokenData) (IngressCursor, error) {
	r := a.pairingRouter()
	if token == nil || token.AccountID == "" || a.token.Load() != token || r == nil || r.ingressStore == nil {
		return IngressCursor{}, errors.New("wechat ingress account unavailable")
	}
	if a.accountLockID != token.AccountID {
		lock, err := AcquireIMAccountLock("wechat", token.AccountID)
		if err != nil {
			return IngressCursor{}, fmt.Errorf("lock Wechat account: %w", err)
		}
		a.closeIngressAccount()
		a.accountLock, a.accountLockID = lock, token.AccountID
	}
	if a.token.Load() != token {
		return IngressCursor{}, errors.New("wechat account changed during preparation")
	}
	if err := r.PrepareIngressAccount(ctx, "wechat", token.AccountID); err != nil {
		return IngressCursor{}, fmt.Errorf("prepare Wechat ingress: %w", err)
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
