package gateway

// Wire types come from the pinned upstream module. Only lifecycle code is forked.
import upstream "github.com/disgoorg/disgo/gateway"

type Message = upstream.Message
type MessageData = upstream.MessageData
type MessageDataHeartbeat = upstream.MessageDataHeartbeat
type MessageDataIdentify = upstream.MessageDataIdentify
type IdentifyCommandDataProperties = upstream.IdentifyCommandDataProperties
type MessageDataPresenceUpdate = upstream.MessageDataPresenceUpdate
type PresenceOpt = upstream.PresenceOpt
type MessageDataResume = upstream.MessageDataResume
type MessageDataInvalidSession = upstream.MessageDataInvalidSession
type MessageDataHello = upstream.MessageDataHello
type EventData = upstream.EventData
type EventUnknown = upstream.EventUnknown
type EventReady = upstream.EventReady
type EventResumed = upstream.EventResumed
type EventChannelCreate = upstream.EventChannelCreate
type EventChannelUpdate = upstream.EventChannelUpdate
type EventChannelDelete = upstream.EventChannelDelete
type EventThreadCreate = upstream.EventThreadCreate
type EventThreadUpdate = upstream.EventThreadUpdate
type EventThreadDelete = upstream.EventThreadDelete
type EventThreadListSync = upstream.EventThreadListSync
type EventMessageCreate = upstream.EventMessageCreate
type EventRaw = upstream.EventRaw
type EventHeartbeatAck = upstream.EventHeartbeatAck
type EventType = upstream.EventType
type Opcode = upstream.Opcode
type Intents = upstream.Intents

const (
	EventTypeRaw           = upstream.EventTypeRaw
	EventTypeHeartbeatAck  = upstream.EventTypeHeartbeatAck
	EventTypeReady         = upstream.EventTypeReady
	EventTypeResumed       = upstream.EventTypeResumed
	EventTypeMessageCreate = upstream.EventTypeMessageCreate
	OpcodeDispatch         = upstream.OpcodeDispatch
	IntentGuilds           = upstream.IntentGuilds
	IntentsNone            = upstream.IntentsNone
)

const (
	OpcodeHeartbeat      = upstream.OpcodeHeartbeat
	OpcodeIdentify       = upstream.OpcodeIdentify
	OpcodePresenceUpdate = upstream.OpcodePresenceUpdate
	OpcodeResume         = upstream.OpcodeResume
	OpcodeReconnect      = upstream.OpcodeReconnect
	OpcodeInvalidSession = upstream.OpcodeInvalidSession
	OpcodeHello          = upstream.OpcodeHello
	OpcodeHeartbeatACK   = upstream.OpcodeHeartbeatACK
	IntentGuildMessages  = upstream.IntentGuildMessages
	IntentDirectMessages = upstream.IntentDirectMessages
	IntentsDefault       = upstream.IntentsDefault
)

func CloseEventCodeByCode(code int) upstream.CloseEventCode {
	return upstream.CloseEventCodeByCode(code)
}
