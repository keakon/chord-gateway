package im

import "time"

// DeliveryTarget names the authoritative destination of one outbound message:
// the chat identity plus the runtime and binding the send is admitted against.
type DeliveryTarget struct {
	Identity                        ChatIdentity
	AccountEpoch, BindingGeneration uint64
	ProcessKey, RuntimeInstanceID   string
	Lease                           *BindingLease
	Paired                          bool
}

// ReplyPermit authorizes one bounded flow reply sent through a transport's
// control channel.
type ReplyPermit struct {
	Identity        ChatIdentity
	SenderID        string
	AccountEpoch    uint64
	RequestID, Kind string
	ExpiresAt       time.Time
	Ticket          AdmissionTicket
}

// ReplyAccountContextKey carries the admitted account identity of a scoped
// reply so the transport can verify the destination account.
type ReplyAccountContextKey struct{}

// OutboundDeliveryContextKey carries the DeliveryTarget a send was admitted
// against; transports must validate their exact destination against it.
type OutboundDeliveryContextKey struct{}

// OutboundReplyContextKey carries the ReplyPermit for a flow reply, or nil for
// ordinary sends.
type OutboundReplyContextKey struct{}
