package main

import "github.com/keakon/chord-gateway/internal/im"

// Freeze the whole observed batch before accepting any item. A slow or retried
// predecessor must not redirect a later control to a newly pending request.
// The snapshot stays in memory with the retained batch, never in the wire data.
func (a *WechatAdapter) capturePollingMessages(response *ilinkGetUpdatesResponse, token *TokenData) {
	r := a.pairingRouter()
	if r == nil {
		return
	}
	for i := range response.Msgs {
		msg := &response.Msgs[i]
		if msg.captureRef != nil {
			continue
		}
		incoming, supported := normalizeWechatIncoming(*msg, token)
		if !supported {
			continue
		}
		kind, target := r.CaptureIngress(incoming)
		msg.captureRef = &im.IngressCapture{Kind: kind, Target: target}
	}
}
