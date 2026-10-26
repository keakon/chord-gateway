package gateway

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

// handleChordCommand dispatches a command to the chord process.
// msg may be nil; in that case a synthetic im.IncomingMessage is built from the
// command's IM type and chat ID (used for command paths like /cancel that
// don't carry the original message context).
func (r *NotificationRouter) handleChordCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, imType string, msg *im.IncomingMessage) {
	incoming := im.IncomingMessage{IMType: imType, ChatID: chatID, SenderID: chatID}
	if msg != nil {
		incoming = *msg
	}
	procKey := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	proc, err := r.mgr.getOrSpawnForKeyChatLocked(procKey)
	if errors.Is(err, errUnresolvedInputs) {
		if original := r.mgr.GetProcessForKey(procKey); cmd.Type == "status" && original != nil {
			r.sendTextForKey(procKey, chatID, "⚠️ Chord disconnected. "+formatBindingStatus(ws, imType, chatID, original.State()))
		} else {
			r.sendTextForKey(procKey, chatID, errUnresolvedInputs.Error())
		}
		return
	}
	if err != nil {
		log.Errorf("failed to get or spawn process workspace=%v error=%v", ws.ID, err)
		r.sendTextForKey(procKey, chatID, "❌ Failed to connect to chord process.")
		return
	}
	if proc == nil {
		log.Errorf("no process for workspace workspace=%v", ws.ID)
		r.sendTextForKey(procKey, chatID, "❌ Workspace not configured.")
		return
	}

	if incoming.InternalAction != nil && proc.bindingInstanceID != "" && incoming.InternalAction.BindingInstanceID != proc.bindingInstanceID {
		r.sendTextForKey(procKey, chatID, "⚠️ This card belongs to a previous process. Use /status.")
		return
	}

	switch cmd.Type {
	case "status":
		r.handleStatusCommand(ws, chatID, imType, proc)
	case "cancel":
		r.handleCancelCommand(ws, chatID, procKey, proc)
	case "confirm":
		r.handleConfirmCommand(ws, chatID, cmd, incoming, procKey, proc)
	case "question":
		r.handleQuestionCommand(ws, chatID, cmd, incoming, procKey, proc)
	case "handoff":
		r.handleHandoffCommand(ws, chatID, cmd, procKey, proc)
	case "role":
		r.handleRoleCommand(ws, chatID, cmd, incoming, procKey, proc)
	case "models":
		r.handleModelsCommand(ws, chatID, cmd, procKey, proc)
	case "image":
		r.handleImageCommand(ws, chatID, cmd, procKey, proc)
	case "queue":
		r.handleQueueCommand(ws, chatID, cmd, procKey, proc)
	case "send":
		r.handleSendCommand(ws, chatID, cmd, incoming, procKey, proc)
	case "local_shell":
		r.handleLocalShellCommand(ws, chatID, cmd, procKey, proc)
	default:
		log.Warnf("unknown command type type=%v", cmd.Type)
		r.sendTextForKey(procKey, chatID, fmt.Sprintf("⚠️ Unknown command: %s", cmd.Type))
	}
}

// handleModelsCommand answers the models protocol command. Without this the
// only way to read or change the model pool from IM was to ask the agent, which
// neither shows the effective pools nor survives a busy turn.
func (r *NotificationRouter) handleModelsCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, procKey string, proc *ChordProcess) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool := strings.TrimSpace(cmd.Content)
	action := "status"
	if pool != "" {
		action = "set_current_model_pool"
	}
	resp, err := proc.WaitModels(ctx, action, pool)
	if err != nil {
		log.Warnf("models command did not receive a response workspace=%v action=%v error=%v", ws.ID, action, err)
		r.sendTextForKey(procKey, chatID, fmt.Sprintf("⚠️ Could not confirm the model pool change. Reply %s to check what Chord reports.", "/status"))
		return
	}
	if !resp.OK {
		r.sendTextForKey(procKey, chatID, "⚠️ "+modelsResponseMessage(resp.Message, "Chord rejected the models request."))
		return
	}
	if action == "set_current_model_pool" {
		log.Infof("models.pool_switch workspace=%v chat_id=%v pool=%v", ws.ID, chatID, pool)
		text := "✅ Switched the current model pool."
		if status := strings.TrimSpace(resp.Status); status != "" {
			text += "\n\n" + status
		}
		r.sendTextForKey(procKey, chatID, text)
		return
	}
	if status := strings.TrimSpace(resp.Status); status != "" {
		r.sendTextForKey(procKey, chatID, status)
		return
	}
	r.sendTextForKey(procKey, chatID, "ℹ️ No model pool status is available from Chord.")
}

// handleImageCommand asks Chord to resend a generated image by artifact
// reference. A transfer that lost chunks used to be unrecoverable from IM — the
// notice said the original was still in Chord but offered no way to get it.
func (r *NotificationRouter) handleImageCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, procKey string, proc *ChordProcess) {
	reference := strings.TrimSpace(cmd.Content)
	if reference == "" {
		r.sendTextForKey(procKey, chatID, "⚠️ Use /image <reference> with the reference from the failed transfer notice.")
		return
	}
	if !strings.HasPrefix(reference, "artifact:images/") {
		r.sendTextForKey(procKey, chatID, "⚠️ That is not an image artifact reference. It should start with artifact:images/.")
		return
	}
	state := proc.State()
	requestID := "im-image-" + rand.Text()
	if !proc.images.expectRefetch(state.SessionID, reference, requestID) {
		r.sendTextForKey(procKey, chatID, "⚠️ A resend for that image is already pending or already arrived.")
		return
	}
	if err := proc.SendCommand(map[string]any{"type": "image_artifact", "content": reference, "request_id": requestID}); err != nil {
		proc.images.cancelRefetch(requestID)
		log.Errorf("failed to request image resend workspace=%v error=%v", ws.ID, err)
		r.sendTextForKey(procKey, chatID, "❌ Failed to ask Chord for the original image.")
		return
	}
	log.Infof("image.resend_requested workspace=%v chat_id=%v reference=%v", ws.ID, chatID, reference)
	r.sendTextForKey(procKey, chatID, "🖼️ Asked Chord for the original image; it arrives here when the transfer finishes.")
}

func modelsResponseMessage(detail, fallback string) string {
	if strings.TrimSpace(detail) == "" {
		return fallback
	}
	return detail
}

func (r *NotificationRouter) handleStatusCommand(ws *config.Workspace, chatID, imType string, proc *ChordProcess) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	state, err := proc.WaitStatus(ctx)
	if err != nil {
		// Either the send failed or the response did not arrive in time.
		// Fall back to whatever state we currently have so the user still gets a reply.
		log.Warnf("status command did not receive a response in time workspace=%v error=%v", ws.ID, err)
		state = proc.State()
	}
	r.sendTextForIM(imType, chatID, formatBindingStatus(ws, imType, chatID, state))
}

func (r *NotificationRouter) handleCancelCommand(ws *config.Workspace, chatID, procKey string, proc *ChordProcess) {
	if err := proc.SendCommand(map[string]any{"type": "cancel"}); err != nil {
		log.Errorf("failed to send cancel command workspace=%v error=%v", ws.ID, err)
		r.sendTextForKey(procKey, chatID, "❌ Failed to cancel.")
		return
	}
	r.scheduleCommandReminder(procKey)
	r.sendTextForKey(procKey, chatID, "🛑 Cancel requested.")
}

func (r *NotificationRouter) handleConfirmCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, msg im.IncomingMessage, procKey string, proc *ChordProcess) {
	state := proc.State()
	pc := state.PendingConfirm
	if pc == nil || strings.TrimSpace(pc.RequestID) == "" {
		expired := r.lookupExpiredPending(procKey)
		content := buildExpiredConfirmFollowup(cmd, expired.Confirm)
		if err := proc.SendUserMessage(content); err != nil {
			log.Errorf("failed to send expired confirmation follow-up workspace=%v error=%v", ws.ID, err)
			r.sendTextForKey(procKey, chatID, "❌ Failed to send follow-up message to chord.")
			return
		}
		r.resolveFeishuCard(msg, procKey, "confirm", cmd.RequestID, "Confirmation expired", "⌛ The pending confirmation has expired.", "grey")
		r.sendTextForKey(procKey, chatID, "⚠️ The pending confirmation has expired. Your response was sent as a follow-up message, not as an approval or denial.")
		return
	}
	requestID := pc.RequestID
	if cmd.RequestID != "" {
		if cmd.RequestID != pc.RequestID {
			r.resolveFeishuCard(msg, procKey, "confirm", cmd.RequestID, "No matching confirmation", "⚠️ This confirmation has already been handled or no longer matches the current request.", "grey")
			r.sendTextForKey(procKey, chatID, "⚠️ No matching pending confirmation to respond to.")
			return
		}
		requestID = cmd.RequestID
	}
	reason := strings.TrimSpace(cmd.Reason)
	if cmd.Action == "deny" && isDoneTool(pc.ToolName) && reason == "" {
		r.sendTextForKey(procKey, chatID, "⚠️ Rejecting Done requires a reason. Reply /deny <what remains to do>.")
		return
	}
	if cmd.Invalid {
		r.sendTextForKey(procKey, chatID, "⚠️ Use /allow [--session|--project|--user] [request-id].")
		return
	}
	rules := pc.RuleCandidates()
	if cmd.RuleScope != "" && len(rules) == 0 {
		// Nothing durable to record: Chord offered no rule for this request, so
		// approving "and remember" would either fail or write a rule nobody chose.
		r.sendTextForKey(procKey, chatID, "⚠️ This request offers no rules to remember. Use /allow to approve just this call.")
		return
	}
	confirmCmd := map[string]any{
		"type":       "confirm",
		"request_id": requestID,
		"action":     cmd.Action,
	}
	if cmd.Action == "deny" && reason != "" {
		confirmCmd["deny_reason"] = reason
	}
	if cmd.RuleScope != "" {
		confirmCmd["rule_scope"] = cmd.RuleScope
		items := make([]map[string]string, 0, len(rules))
		for _, rule := range rules {
			items = append(items, map[string]string{"tool": rule.Tool, "pattern": rule.Pattern})
		}
		confirmCmd["rule_items"] = items
	}
	var ruleReceipt ConfirmRuleIntentResult
	var ruleReceiptErr error
	if cmd.RuleScope != "" {
		ctx, cancel := context.WithTimeout(context.Background(), confirmRuleIntentWaitTimeout)
		var sent bool
		ruleReceipt, sent, ruleReceiptErr = proc.WaitConfirmRuleIntentResult(ctx, requestID, confirmCmd)
		cancel()
		if !sent {
			log.Errorf("failed to send confirm response workspace=%v error=%v", ws.ID, ruleReceiptErr)
			r.sendTextForKey(procKey, chatID, "❌ Failed to send confirmation.")
			return
		}
	} else if err := proc.SendCommand(confirmCmd); err != nil {
		log.Errorf("failed to send confirm response workspace=%v error=%v", ws.ID, err)
		r.sendTextForKey(procKey, chatID, "❌ Failed to send confirmation.")
		return
	}
	r.scheduleCommandReminder(procKey)
	sender := displaySender(msg)
	if cmd.Action == "deny" {
		text := "✅ denied"
		status := "❌ Denied by " + sender
		if reason != "" {
			text += ": " + reason
			status += ": " + reason
		}
		r.resolveFeishuCard(msg, procKey, "confirm", requestID, "Confirmation denied", status, "red")
		log.Infof("confirm.responded workspace=%v chat_id=%v sender_id=%v request_id=%v action=%v tool=%v", ws.ID, chatID, msg.SenderID, requestID, cmd.Action, pc.ToolName)
		r.sendTextForKey(procKey, chatID, text)
		return
	}
	r.resolveFeishuCard(msg, procKey, "confirm", requestID, "Confirmation approved", "✅ Approved by "+sender, "green")
	log.Infof("confirm.responded workspace=%v chat_id=%v sender_id=%v request_id=%v action=%v tool=%v rules=%v rule_scope=%v", ws.ID, chatID, msg.SenderID, requestID, cmd.Action, pc.ToolName, len(rules), cmd.RuleScope)
	if cmd.RuleScope == "" {
		r.sendTextForKey(procKey, chatID, "✅ allowed")
		return
	}
	if ruleReceiptErr != nil {
		log.Warnf("confirm rule persistence result unconfirmed workspace=%v request_id=%v error=%v", ws.ID, requestID, ruleReceiptErr)
		r.sendTextForKey(procKey, chatID, "✅ allowed for this call; could not confirm whether the requested rules were saved. Check the rules on the Chord host before relying on them.")
		return
	}
	r.sendTextForKey(procKey, chatID, formatConfirmRuleIntentReceipt(ruleReceipt, len(rules), cmd.RuleScope))
}

// confirmRuleScopeLabel renders each rule scope the way the operator sees it in
// Chord so the receipt and the durable permission file agree.
func confirmRuleScopeLabel(scope string) string {
	switch scope {
	case "project":
		return "project"
	case "user_global":
		return "user-wide"
	default:
		return "session"
	}
}

func (r *NotificationRouter) handleQuestionCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, msg im.IncomingMessage, procKey string, proc *ChordProcess) {
	if cmd.Invalid {
		r.sendTextForKey(procKey, chatID, "⚠️ Use /question-replace <old-id> <new-id>.")
		return
	}
	state := proc.State()
	if cmd.RequestID != "" && state.Questions[cmd.RequestID] == nil {
		// Restored terminal questions are only available through a snapshot;
		// open-question pushes may also still be draining during process startup.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var err error
		state, err = proc.WaitStatus(ctx, cmd.RequestID)
		cancel()
		if err != nil {
			r.sendTextForKey(procKey, chatID, "⚠️ Could not synchronize questions. Check /status before retrying.")
			return
		}
	}
	q := state.Questions[cmd.RequestID]
	if cmd.RequestID == "" {
		q = state.uniquePendingQuestion()
	}
	if q == nil {
		switch {
		case cmd.RequestID != "":
			r.sendTextForKey(procKey, chatID, "⚠️ Question "+cmd.RequestID+" was not found in the current session. Check /status and the session ID.")
		case state.pendingQuestionCount() == 0:
			r.sendTextForKey(procKey, chatID, "⚠️ No pending question. Use /answer @<id> <answer> to revise a historical answer.")
		default:
			r.sendTextForKey(procKey, chatID, "⚠️ Specify a question ID when several questions are available: /answer @<id> <answer>.")
		}
		return
	}
	if cmd.Action == "interact" || cmd.Action == "withdraw_requirement" || cmd.Action == "replace_requirement" {
		action, err := questionOperationCommand(msg, q, cmd, &im.IngressControlTarget{RequestID: q.RequestID, Version: q.Version})
		if err != nil {
			r.sendTextForKey(procKey, chatID, "⚠️ Invalid question action.")
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		receipt, err := r.waitQuestionOperation(ctx, procKey, msg.SenderID, proc, action)
		if err != nil {
			r.sendTextForKey(procKey, chatID, "⚠️ Question outcome unknown; check /status before retrying.")
			return
		}
		if !receipt.Accepted {
			r.sendTextForKey(procKey, chatID, "⚠️ "+receipt.Error)
			return
		}
		r.sendTextForKey(procKey, chatID, "✅ Question updated.")
		return
	}
	r.submitQuestionAnswer(chatID, msg, procKey, proc, q, cmd.Answers)
}

func questionUserOperationID(msg im.IncomingMessage, id, action string) string {
	source := msg.MessageID
	if source == "" {
		source = fmt.Sprint(time.Now().UnixNano())
	}
	return "gateway:" + msg.IMType + ":" + msg.ChatID + ":" + source + ":" + id + ":" + action
}

func (r *NotificationRouter) handleHandoffCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, procKey string, proc *ChordProcess) {
	if cmd.Invalid {
		r.sendTextForKey(procKey, chatID, "⚠️ Usage: /handoff <agent> [model_pool] or /handoff-deny <reason>.")
		return
	}
	state := proc.State()
	h := state.PendingHandoff
	if h == nil || strings.TrimSpace(h.RequestID) == "" {
		r.sendTextForKey(procKey, chatID, "⚠️ No pending handoff to respond to.")
		return
	}
	handoffCmd := map[string]any{
		"type":       "handoff",
		"request_id": h.RequestID,
		"action":     cmd.Action,
	}
	if strings.TrimSpace(cmd.Agent) != "" {
		handoffCmd["agent"] = strings.TrimSpace(cmd.Agent)
	}
	if strings.TrimSpace(cmd.Pool) != "" {
		handoffCmd["pool"] = strings.TrimSpace(cmd.Pool)
	}
	if cmd.Action == "deny" && strings.TrimSpace(cmd.Reason) != "" {
		handoffCmd["deny_reason"] = strings.TrimSpace(cmd.Reason)
	}
	if err := proc.SendCommand(handoffCmd); err != nil {
		log.Errorf("failed to send handoff response workspace=%v error=%v", ws.ID, err)
		r.sendTextForKey(procKey, chatID, "❌ Failed to send handoff response.")
		return
	}
	r.scheduleCommandReminder(procKey)
	if cmd.Action == "cancel" {
		// Cancel reports nothing back to the model, so the receipt must not
		// imply a rejection was recorded.
		r.sendTextForKey(procKey, chatID, "↩️ handoff request withdrawn")
		return
	}
	if cmd.Action == "deny" {
		if strings.TrimSpace(cmd.Reason) != "" {
			r.sendTextForKey(procKey, chatID, "✅ handoff denied: "+strings.TrimSpace(cmd.Reason))
			return
		}
		r.sendTextForKey(procKey, chatID, "✅ handoff denied")
		return
	}
	agentName := strings.TrimSpace(cmd.Agent)
	if agentName == "" {
		agentName = defaultHandoffAgentName(h.Agents)
	}
	if strings.TrimSpace(cmd.Pool) != "" {
		r.sendTextForKey(procKey, chatID, fmt.Sprintf("✅ handoff accepted: %s (%s)", agentName, strings.TrimSpace(cmd.Pool)))
		return
	}
	r.sendTextForKey(procKey, chatID, "✅ handoff accepted: "+agentName)
}

func defaultHandoffAgentName(options []HandoffAgentOption) string {
	for _, opt := range options {
		if opt.Default && strings.TrimSpace(opt.Name) != "" {
			return strings.TrimSpace(opt.Name)
		}
	}
	for _, opt := range options {
		if strings.TrimSpace(opt.Name) != "" {
			return strings.TrimSpace(opt.Name)
		}
	}
	return "builder"
}

func (r *NotificationRouter) handleSendCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, msg im.IncomingMessage, procKey string, proc *ChordProcess) {
	if action, ok := remoteMCPCommand(cmd.Content); ok {
		if action == "status" {
			text := formatMCPStatus(proc.State().MCP)
			if text == "" {
				text = "No MCP status is available. Use /status to synchronize with the host."
			}
			r.sendTextForKey(procKey, chatID, text)
		} else {
			r.sendTextForKey(procKey, chatID, "Manage MCP authentication in this session's working directory on the host: chord mcp login <server> or chord mcp logout <server>. Reconnect in the local TUI. These commands are not forwarded to the model.")
		}
		return
	}
	state := proc.State()
	if state.PendingConfirm != nil && isDoneTool(state.PendingConfirm.ToolName) && !strings.HasPrefix(strings.TrimSpace(cmd.Content), "/") {
		r.handleConfirmCommand(ws, chatID, im.IMCommand{Type: "confirm", Action: "deny", Reason: cmd.Content}, msg, procKey, proc)
		return
	}
	// Refuse slash commands Chord would run locally on the main agent. Their
	// effects never reach the headless protocol, so forwarding them would let
	// an IM caller mutate a session the gateway can no longer observe — /yolo
	// in particular turns permission confirmations off silently.
	switch content := strings.ToLower(strings.TrimSpace(cmd.Content)); {
	case isChordLocalOnlySlashCommand(content):
		r.sendTextForKey(procKey, chatID, "⚠️ This command is only available in local TUI.")
		return
	case isGatewayOwnedSlashCommand(content):
		r.sendTextForKey(procKey, chatID, "⚠️ This command belongs to the gateway; send it without extra text.")
		return
	}
	if err := proc.SendUserMessage(cmd.Content); err != nil {
		log.Errorf("failed to send user message workspace=%v error=%v", ws.ID, err)
		if !errors.Is(err, ErrSessionTransferred) && r.retrySendInFreshSession(ws, chatID, procKey, cmd.Content) {
			return
		}
		r.sendTextForKey(procKey, chatID, "❌ Failed to send message to chord.")
		return
	}
}

func (r *NotificationRouter) handleLocalShellCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, procKey string, proc *ChordProcess) {
	command := strings.TrimSpace(cmd.Content)
	if command == "" {
		r.sendTextForKey(procKey, chatID, "⚠️ Empty command after !")
		return
	}
	if err := proc.SendCommand(map[string]any{"type": "local_shell", "command": command}); err != nil {
		log.Errorf("failed to send local shell command workspace=%v error=%v", ws.ID, err)
		r.sendTextForKey(procKey, chatID, "❌ Failed to run local shell command.")
		return
	}
	r.scheduleCommandReminder(procKey)
}

func (r *NotificationRouter) retrySendInFreshSession(ws *config.Workspace, chatID, procKey, content string) bool {
	if r.mgr == nil || r.mgr.transferBlocked(procKey) {
		return false
	}
	proc, err := r.mgr.replaceOrdinarySession(procKey, "")
	if err != nil {
		log.Errorf("failed to start fresh chord session for retry workspace=%v error=%v", ws.ID, err)
		return false
	}
	if proc == nil {
		log.Errorf("no process for retry workspace=%v", ws.ID)
		return false
	}
	if err := proc.SendUserMessage(content); err != nil {
		log.Errorf("failed to send user message to fresh session workspace=%v error=%v", ws.ID, err)
		return false
	}
	r.sendTextForKey(procKey, chatID, "⚠️ Previous Chord session was not found or is busy. Started a new session and sent your message.")
	return true
}

// submitQuestionAnswer reports acceptance only after a correlated Core receipt.
func (r *NotificationRouter) submitQuestionAnswer(chatID string, msg im.IncomingMessage, procKey string, proc *ChordProcess, q *QuestionPayload, answers []string) bool {
	// User text is always custom, even if it matches an option ID or label.
	// Only numeric selections or trusted option/form callbacks select IDs.
	answer := parseQuestionAnswer(answers, q, msg.InternalAction)
	if len(answer.values) == 0 {
		r.sendTextForKey(procKey, chatID, "⚠️ This selection no longer matches the question. Check /status and try again.")
		return false
	}
	cmd, err := questionOperationCommand(msg, q, im.IMCommand{Type: "question", Answers: answers}, &im.IngressControlTarget{RequestID: q.RequestID, Version: q.Version})
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, err := r.waitQuestionOperation(ctx, procKey, msg.SenderID, proc, cmd)
	if err != nil {
		r.sendTextForKey(procKey, chatID, "⚠️ Answer outcome unknown; check /status before retrying.")
		return false
	}
	if !receipt.Accepted {
		r.sendTextForKey(procKey, chatID, "⚠️ Answer not accepted: "+receipt.Error)
		return false
	}
	r.sendTextForKey(procKey, chatID, truncate("✅ Answer accepted ("+q.RequestID+"): "+strings.Join(answer.labels, ", ")))
	return true
}

// roleListTimeout and roleSwitchTimeout bound the two /role chord round-trips.
// They are package variables so tests can shorten them instead of waiting out
// the real timeout.
var (
	roleListTimeout   = 10 * time.Second
	roleSwitchTimeout = 10 * time.Second
)

// handleRoleCommand lists the switchable main-agent roles or switches to one.
// The target comes from a plain /role reply (number or name) or from a Feishu
// role-card button (im.InternalAction.Value carries the role name, matched by
// exact name and never as a menu number). Every switch
// refetches the role list from chord so menu numbers and the current-role
// check reflect the live configuration instead of a stale menu snapshot.
func (r *NotificationRouter) handleRoleCommand(ws *config.Workspace, chatID string, cmd im.IMCommand, incoming im.IncomingMessage, procKey string, proc *ChordProcess) {
	listCtx, listCancel := context.WithTimeout(context.Background(), roleListTimeout)
	defer listCancel()

	listResp, err := proc.WaitRoleList(listCtx)
	if err != nil {
		log.Warnf("role list failed workspace=%v error=%v", ws.ID, err)
		r.replyRoleResult(chatID, incoming, procKey, "Role unavailable", "❌ Failed to load available roles.", "grey")
		return
	}
	if !listResp.OK {
		r.replyRoleResult(chatID, incoming, procKey, "Role unavailable", "⚠️ "+roleResponseMessage(listResp.Message, "Failed to load available roles."), "grey")
		return
	}
	current, roles := roleListState(listResp)
	if len(roles) == 0 {
		r.replyRoleResult(chatID, incoming, procKey, "Role unavailable", "⚠️ No switchable roles are configured.", "grey")
		return
	}

	target := strings.TrimSpace(cmd.Content)
	if target == "" {
		r.showRoleMenu(chatID, procKey, current, roles, incoming, proc.bindingInstanceID)
		return
	}

	name, err := selectRoleName(target, roles, incoming.InternalAction != nil && incoming.InternalAction.Type == "role")
	if err != nil {
		text := err.Error()
		text = strings.ToUpper(text[:1]) + text[1:] + "."
		r.replyRoleResult(chatID, incoming, procKey, "Role unavailable", "⚠️ "+text, "grey")
		return
	}

	if name == current {
		r.replyRoleResult(chatID, incoming, procKey, "Role unchanged", fmt.Sprintf("ℹ️ %s is already the current role.", name), "grey")
		return
	}

	setCtx, setCancel := context.WithTimeout(context.Background(), roleSwitchTimeout)
	defer setCancel()

	setResp, err := proc.WaitRoleSwitch(setCtx, name)
	if err != nil {
		log.Warnf("role switch failed workspace=%v role=%v error=%v", ws.ID, name, err)
		r.replyRoleResult(chatID, incoming, procKey, "Role switch unconfirmed", "Could not confirm the role switch. Send /status to check the current role.", "grey")
		return
	}
	if !setResp.OK {
		r.replyRoleResult(chatID, incoming, procKey, "Role switch rejected", "⚠️ "+roleResponseMessage(setResp.Message, "Role switch was rejected."), "red")
		return
	}
	log.Infof("role.switched workspace=%v chat_id=%v role=%v", ws.ID, chatID, name)
	r.replyRoleResult(chatID, incoming, procKey, "Role switched", "✅ Switched role: "+name, "green")
}

// showRoleMenu presents the role list, preferring a Feishu interactive card
// and falling back to a numbered text menu. When a Feishu card send fails the
// router's sendCardToTarget has already emitted the text fallback, so the
// handler only falls through to sendText when no Feishu adapter is attached.
func (r *NotificationRouter) showRoleMenu(chatID, procKey, current string, roles []RoleInfo, incoming im.IncomingMessage, instance ...string) {
	if config.NormalizeIMType(incoming.IMType) == "feishu" {
		if r.sendFeishuRoleMenu(chatID, procKey, current, roles, instance...) {
			return
		}
		if r.findFeishuCardAdapter() != nil {
			return
		}
	}
	r.sendTextForKey(procKey, chatID, buildRoleMenuText(current, roles))
}

// replyRoleResult reports a /role outcome. A Feishu card click is patched in
// place and does not also send a duplicate chat line; text platforms (and
// Feishu when the card cannot be patched) still get sendText.
func (r *NotificationRouter) replyRoleResult(chatID string, incoming im.IncomingMessage, procKey, title, message, template string) {
	if r.resolveRoleCard(incoming, procKey, title, message, template) {
		return
	}
	r.sendTextForKey(procKey, chatID, message)
}

// resolveRoleCard patches the Feishu role menu card when the switch was
// triggered by a card button. It is a no-op for text replies, and reports
// whether a card was actually patched so callers can skip a duplicate chat
// message.
func (r *NotificationRouter) resolveRoleCard(incoming im.IncomingMessage, procKey, title, message, template string) bool {
	if incoming.InternalAction == nil || incoming.InternalAction.RequestID == "" {
		return false
	}
	return r.resolveFeishuCard(incoming, procKey, "role", incoming.InternalAction.RequestID, title, message, template)
}

// buildRoleMenuText renders the numbered role list used by text platforms and
// as the card fallback.
func buildRoleMenuText(current string, roles []RoleInfo, hints ...func(string) string) string {
	var sb strings.Builder
	if strings.TrimSpace(current) != "" {
		fmt.Fprintf(&sb, "🎭 Current role: %s\n", strings.TrimSpace(current))
	} else {
		sb.WriteString("🎭 Select a role:\n")
	}
	for i, r := range roles {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			continue
		}
		line := fmt.Sprintf("%d. %s", i+1, name)
		if name == current {
			line += " (current)"
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("Reply ")
	sb.WriteString(commandHint("/role <number>", hints))
	sb.WriteString(" or ")
	sb.WriteString(commandHint("/role <name>", hints))
	sb.WriteString(" to switch.")
	return truncate(sb.String())
}

// roleListState extracts the current role and the non-empty role list from a
// role_response. The current role is taken from the payload's role field and
// falls back to the entry marked current.
func roleListState(resp RoleResponse) (string, []RoleInfo) {
	roles := make([]RoleInfo, 0, len(resp.Roles))
	for _, r := range resp.Roles {
		if strings.TrimSpace(r.Name) != "" {
			roles = append(roles, RoleInfo{Name: strings.TrimSpace(r.Name), Current: r.Current})
		}
	}
	current := strings.TrimSpace(resp.Role)
	if current == "" {
		for _, r := range roles {
			if r.Current {
				current = r.Name
				break
			}
		}
	}
	return current, roles
}

func roleNameAvailable(name string, roles []RoleInfo) bool {
	for _, r := range roles {
		if r.Name == name {
			return true
		}
	}
	return false
}

// roleResponseMessage returns the chord-provided rejection message, falling
// back to a generic message when the response carries none.
func roleResponseMessage(message, fallback string) string {
	if strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	return fallback
}
