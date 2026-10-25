package main

import (
	"context"
	"errors"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/internal/im"
)

// ImageSender is implemented by adapters with an outbound media contract.
type ImageSender interface {
	SendImage(string, *im.ReceivedImage) error
}

func (r *NotificationRouter) HandleChordImage(key string, image *im.ReceivedImage) {
	target, allowed := r.targetForKey(key)
	if !allowed {
		image.Close()
		return
	}
	ctx := context.Background()
	if target.lease != nil {
		ctx = target.lease.Context()
	}
	_, imType, chatID := parseProcessKey(key)
	adapter := r.findAdapterByType(imType)
	sender, ok := adapter.(ImageSender)
	if !ok || chatID == "" {
		image.Close()
		return
	}
	task := func() {
		defer image.Close()
		if image.BindingInstanceID != "" && image.BindingInstanceID != target.RuntimeInstanceID {
			return
		}
		ctx, done, err := r.deliveryContext(ctx, target)
		if err != nil {
			return
		}
		defer done()
		send := func() error {
			if contextual, ok := sender.(interface {
				SendImageContext(context.Context, string, *im.ReceivedImage) error
			}); ok {
				return contextual.SendImageContext(ctx, chatID, image)
			}
			if target.lease != nil {
				return context.Canceled
			}
			return sender.SendImage(chatID, image)
		}
		if err := send(); err != nil {
			log.Errorf("image delivery failed session=%v call=%v reference=%v error=%v", image.SessionID, image.CallID, image.Reference, err)
			notice := "Image delivery failed. The original is saved in the Chord session and can be viewed there; do not regenerate."
			if errors.Is(err, im.ErrDeliveryOutcomeUnknown) {
				notice = "Image delivery is unconfirmed; check the chat first. The original is saved in the Chord session and is not resent or regenerated automatically."
			}
			if err := r.SendBound(ctx, target, notice); err != nil {
				log.Errorf("image delivery notice failed session=%v call=%v error=%v", image.SessionID, image.CallID, err)
			} else {
				r.markVisibleOutput(key)
			}
		} else {
			r.markVisibleOutput(key)
		}
	}
	if r.outbound != nil {
		if r.outbound.enqueueOrWaitContext(ctx, key, task) == outboundClosed {
			image.Close()
		}
		return
	}
	task()
}
