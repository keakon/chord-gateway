package im

import "context"

// InteractiveCardAdapter is implemented by transports that can deliver and
// patch interactive cards. The gateway looks it up per platform and treats a
// failed lookup as "this platform has no card support".
type InteractiveCardAdapter interface {
	// SendInteractiveCard posts one interactive card to the chat and returns
	// the platform handle of the created message.
	SendInteractiveCard(ctx context.Context, chatID string, card map[string]any) (*InteractiveCardHandle, error)
	// UpdateInteractiveCard patches an existing card message in place.
	UpdateInteractiveCard(ctx context.Context, messageID string, card map[string]any) error
}
