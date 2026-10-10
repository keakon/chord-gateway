package main

import (
	"errors"

	"github.com/keakon/golog/log"
)

// ImageSender is implemented by adapters with an outbound media contract.
type ImageSender interface {
	SendImage(string, *ReceivedImage) error
}

func (r *NotificationRouter) HandleChordImage(key string, image *ReceivedImage) {
	_, imType, chatID := parseProcessKey(key)
	adapter := r.findAdapterByType(imType)
	sender, ok := adapter.(ImageSender)
	if !ok || chatID == "" {
		image.Close()
		return
	}
	task := func() {
		defer image.Close()
		if err := sender.SendImage(chatID, image); err != nil {
			log.Errorf("image delivery failed session=%v call=%v reference=%v error=%v", image.SessionID, image.CallID, image.Reference, err)
			notice := "Image delivery failed. The original is saved in the Chord session and can be viewed there; do not regenerate."
			if errors.Is(err, errDeliveryOutcomeUnknown) {
				notice = "Image delivery is unconfirmed; check the chat first. The original is saved in the Chord session and is not resent or regenerated automatically."
			}
			if err := adapter.SendText(chatID, notice); err != nil {
				log.Errorf("image delivery notice failed session=%v call=%v error=%v", image.SessionID, image.CallID, err)
			} else {
				r.markVisibleOutput(key)
			}
		} else {
			r.markVisibleOutput(key)
		}
	}
	if r.outbound != nil {
		if r.outbound.enqueueOrWait(key, task) == outboundClosed {
			image.Close()
		}
		return
	}
	task()
}
