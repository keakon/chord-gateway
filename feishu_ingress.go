package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// These submitters run on the reader. Submission only reserves bounded shared
// worker capacity; the future carries the authoritative in-memory outcome.
func (a *FeishuAdapter) submitMessageEvent(ctx context.Context, event *larkim.P2MessageReceiveV1, deliveryID string) (<-chan ReceiveResult, error) {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil || event.Event.Sender.SenderId == nil {
		return nil, nil
	}
	fc := a.feishuConfig()
	if fc == nil || !a.matchesActiveAccount(fc.AppID) || derefString(event.Event.Sender.SenderType) != "user" || !feishuEventMatchesApp(event.EventV2Base, fc.AppID) {
		return nil, nil
	}
	message := event.Event.Message
	text, err := parseFeishuMessageText(derefString(message.MessageType), derefString(message.Content))
	if err != nil || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	router, ok := a.msgRouter.(*NotificationRouter)
	if !ok || router == nil {
		return nil, errIngressClosed
	}
	msg := IncomingMessage{IMType: "feishu", AccountID: fc.AppID, AppID: fc.AppID, ChatKind: feishuChatKind(derefString(message.ChatType)), ChatID: derefString(message.ChatId), SenderID: derefString(event.Event.Sender.SenderId.OpenId), Text: text}
	if id := derefString(message.MessageId); id != "" {
		msg.MessageID = "message:" + id
	}
	source := feishuSourceTime(derefString(message.CreateTime))
	if source.IsZero() {
		source = feishuHeaderTime(event.EventV2Base)
	}
	return router.SubmitIngress(ctx, IngressEnvelope{Message: msg, DeliveryID: deliveryID, SourceTime: source, ReceivedAt: time.Now()})
}

func (a *FeishuAdapter) submitCardActionEvent(ctx context.Context, event *larkcallback.CardActionTriggerEvent, deliveryID string) (<-chan ReceiveResult, *larkcallback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil || event.Event.Context == nil {
		return nil, nil, nil
	}
	fc := a.feishuConfig()
	if fc == nil || !a.matchesActiveAccount(fc.AppID) || !feishuEventMatchesApp(event.EventV2Base, fc.AppID) {
		return nil, nil, nil
	}
	action := event.Event.Action
	chatID := event.Event.Context.OpenChatID
	senderID := ""
	if event.Event.Operator != nil {
		senderID = event.Event.Operator.OpenID
	}
	requestID, _ := action.Value["request_id"].(string)
	actionType, _ := action.Value["type"].(string)
	actionName, _ := action.Value["action"].(string)
	value, _ := action.Value["value"].(string)
	contextIMType, _ := action.Value["im_type"].(string)
	contextChatID, _ := action.Value["chat_id"].(string)
	if requestID == "" || actionType == "" || contextChatID == "" || contextIMType != "feishu" || contextChatID != chatID {
		return nil, nil, nil
	}
	answers, err := feishuQuestionFormAnswers(action.FormValue)
	if err != nil {
		return nil, feishuCardToast("error", "Could not read your selection. Please reply with /answer instead."), nil
	}
	if action.Name == feishuQuestionSubmitName && len(answers) == 0 {
		return nil, feishuCardToast("warning", "Select at least one option, then submit."), nil
	}
	if !isValidFeishuCardAction(actionType, actionName, value, answers) {
		return nil, nil, nil
	}
	if event.EventV2Base == nil || event.EventV2Base.Header == nil || event.EventV2Base.Header.EventID == "" {
		return nil, feishuCardToast("error", "Could not identify this interaction. Please check /status."), nil
	}
	bindingID, _ := action.Value["binding_instance_id"].(string)
	version := feishuCardVersion(action.Value["version"])
	msg := IncomingMessage{IMType: "feishu", AccountID: fc.AppID, AppID: fc.AppID, ChatID: chatID, SenderID: senderID, MessageID: "card:" + event.EventV2Base.Header.EventID, InternalAction: &InternalAction{BindingInstanceID: bindingID, Type: actionType, Action: actionName, RequestID: requestID, Value: value, Values: answers, Version: version, Handle: InteractiveCardHandle{MessageID: event.Event.Context.OpenMessageID, Token: event.Event.Token}}}
	router, ok := a.msgRouter.(*NotificationRouter)
	if !ok || router == nil {
		return nil, nil, errIngressClosed
	}
	envelope := IngressEnvelope{Message: msg, DeliveryID: deliveryID, SourceTime: feishuHeaderTime(event.EventV2Base), ReceivedAt: time.Now()}
	envelope = router.CaptureIngressEnvelope(envelope)
	// Even a legacy callback without a version must retain its original zero
	// version; it cannot acquire a revised question's current version.
	if actionType == "question" && envelope.capture.target != nil {
		envelope.capture.target.Version = version
	}
	future, err := router.SubmitIngress(ctx, envelope)
	if err != nil {
		return nil, feishuCardToast("error", "Not received. Please try again later."), err
	}
	return future, nil, nil
}

func feishuMessageIngressError(result ReceiveResult) error {
	switch result.Disposition {
	case ReceiveAccepted, ReceiveDuplicate, ReceiveRejected:
		return nil
	}
	return fmt.Errorf("feishu ingress %s: %s", result.Disposition, result.Reason)
}

// Recheck output authorization after waiting: config ownership may have changed
// while an in-memory duplicate lookup was running. Its state is owner-only evidence.
func (a *FeishuAdapter) feishuCardIngressResponse(event *larkcallback.CardActionTriggerEvent, result ReceiveResult) *larkcallback.CardActionTriggerResponse {
	fc := a.feishuConfig()
	authorized := fc != nil && fc.AppID != "" && a.matchesActiveAccount(fc.AppID) && fc.Owner != nil && fc.Owner.AccountID == fc.AppID && fc.Owner.UserID != "" && event != nil && event.Event != nil && event.Event.Operator != nil && fc.Owner.UserID == event.Event.Operator.OpenID && feishuEventMatchesApp(event.EventV2Base, fc.AppID)
	if authorized {
		context := event.Event.Context
		action := event.Event.Action
		authorized = context != nil && context.OpenChatID != "" && action != nil && action.Value["chat_id"] == context.OpenChatID && action.Value["im_type"] == "feishu"
	}
	if !authorized {
		return feishuCardToast("warning", "This interaction could not be accepted.")
	}
	return feishuCardIngressResponse(event, result)
}

func feishuCardIngressResponse(event *larkcallback.CardActionTriggerEvent, result ReceiveResult) *larkcallback.CardActionTriggerResponse {
	switch result.Disposition {
	case ReceiveAccepted:
		return feishuCardToast("info", "Received. Processing...")
	case ReceiveDuplicate:
		switch result.State {
		case "rejected", "expired":
			return feishuCardToast("warning", "This interaction was previously rejected or expired. Check /status.")
		case "unknown":
			return feishuCardToast("warning", "The result is still unknown. Check /status before repeating it.")
		case "consumed", "completed":
			return feishuCardToast("info", "This interaction has already been processed.")
		}
		return feishuCardToast("info", "This interaction was already received. Check /status for its result.")
	case ReceiveRejected:
		return feishuCardToast("warning", "This interaction could not be accepted. Check /status for the current request.")
	case ReceiveUncertain:
		return feishuCardToast("warning", "Receipt could not be confirmed. Check /status before repeating this action.")
	default:
		return feishuCardToast("error", "Not received. Please try again later.")
	}
}

func feishuCardToast(kind, text string) *larkcallback.CardActionTriggerResponse {
	return &larkcallback.CardActionTriggerResponse{Toast: &larkcallback.Toast{Type: kind, Content: text}}
}
func feishuEventMatchesApp(base *larkevent.EventV2Base, appID string) bool {
	return base == nil || base.Header == nil || base.Header.AppID == "" || base.Header.AppID == appID
}
func feishuHeaderTime(base *larkevent.EventV2Base) time.Time {
	if base == nil || base.Header == nil {
		return time.Time{}
	}
	return feishuSourceTime(base.Header.CreateTime)
}
func feishuSourceTime(value string) time.Time {
	millis, err := strconv.ParseInt(value, 10, 64)
	if err != nil || millis <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(millis)
}
func feishuChatKind(value string) string {
	switch value {
	case "p2p":
		return "private"
	case "group", "topic", "topic_group":
		return "group"
	}
	return ""
}
func feishuCardVersion(value any) uint64 {
	switch value := value.(type) {
	case string:
		n, _ := strconv.ParseUint(value, 10, 64)
		return n
	case json.Number:
		n, _ := strconv.ParseUint(string(value), 10, 64)
		return n
	case float64:
		if value > 0 && value < float64(^uint64(0)) && value == float64(uint64(value)) {
			return uint64(value)
		}
	case uint64:
		return value
	}
	return 0
}
