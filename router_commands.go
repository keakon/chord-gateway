package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
)

// handleChordCommand dispatches a command to the chord process.
// msg may be nil; in that case a synthetic IncomingMessage is built from the
// command's IM type and chat ID (used for command paths like /cancel that
// don't carry the original message context).
func (r *NotificationRouter) handleChordCommand(ws *config.Workspace, chatID string, cmd IMCommand, imType string, msg *IncomingMessage) {
	incoming := IncomingMessage{IMType: imType, ChatID: chatID, SenderID: chatID}
	if msg != nil {
		incoming = *msg
	}
	procKey := (processKey{workspaceID: ws.ID, imType: imType, chatID: chatID}).String()
	proc, err := r.mgr.GetOrSpawnForKey(procKey)
	if err != nil {
		log.Errorf("failed to get or spawn process workspace=%v error=%v", ws.ID, err)
		r.sendText(chatID, "❌ Failed to connect to chord process.")
		return
	}
	if proc == nil {
		log.Errorf("no process for workspace workspace=%v", ws.ID)
		r.sendText(chatID, "❌ Workspace not configured.")
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
	case "send":
		r.handleSendCommand(ws, chatID, cmd, incoming, procKey, proc)
	case "local_shell":
		r.handleLocalShellCommand(ws, chatID, cmd, procKey, proc)
	default:
		log.Warnf("unknown command type type=%v", cmd.Type)
		r.sendText(chatID, fmt.Sprintf("⚠️ Unknown command: %s", cmd.Type))
	}
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
	r.sendText(chatID, formatBindingStatus(ws, imType, chatID, state))
}

func (r *NotificationRouter) handleCancelCommand(ws *config.Workspace, chatID, procKey string, proc *ChordProcess) {
	if err := proc.SendCommand(map[string]any{"type": "cancel"}); err != nil {
		log.Errorf("failed to send cancel command workspace=%v error=%v", ws.ID, err)
		r.sendText(chatID, "❌ Failed to cancel.")
		return
	}
	r.beginTurn(procKey)
	r.sendText(chatID, "🛑 Cancel requested.")
}

func (r *NotificationRouter) handleConfirmCommand(ws *config.Workspace, chatID string, cmd IMCommand, msg IncomingMessage, procKey string, proc *ChordProcess) {
	state := proc.State()
	pc := state.PendingConfirm
	if pc == nil || strings.TrimSpace(pc.RequestID) == "" {
		expired := r.lookupExpiredPending(procKey)
		content := buildExpiredConfirmFollowup(cmd, expired.Confirm)
		if err := proc.SendUserMessage(content); err != nil {
			log.Errorf("failed to send expired confirmation follow-up workspace=%v error=%v", ws.ID, err)
			r.sendText(chatID, "❌ Failed to send follow-up message to chord.")
			return
		}
		r.beginTurn(procKey)
		r.resolveFeishuCard(msg, procKey, "confirm", cmd.RequestID, "Confirmation expired", "⌛ The pending confirmation has expired.", "grey")
		r.sendText(chatID, "⚠️ The pending confirmation has expired. Your response was sent as a follow-up message, not as an approval or denial.")
		return
	}
	requestID := pc.RequestID
	if cmd.RequestID != "" {
		if cmd.RequestID != pc.RequestID {
			r.resolveFeishuCard(msg, procKey, "confirm", cmd.RequestID, "No matching confirmation", "⚠️ This confirmation has already been handled or no longer matches the current request.", "grey")
			r.sendText(chatID, "⚠️ No matching pending confirmation to respond to.")
			return
		}
		requestID = cmd.RequestID
	}
	reason := strings.TrimSpace(cmd.Reason)
	if cmd.Action == "deny" && isDoneTool(pc.ToolName) && reason == "" {
		r.sendText(chatID, "⚠️ Rejecting Done requires a reason. Reply /deny <what remains to do>.")
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
	if err := proc.SendCommand(confirmCmd); err != nil {
		log.Errorf("failed to send confirm response workspace=%v error=%v", ws.ID, err)
		r.sendText(chatID, "❌ Failed to send confirmation.")
		return
	}
	r.beginTurn(procKey)
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
		r.sendText(chatID, text)
		return
	}
	r.resolveFeishuCard(msg, procKey, "confirm", requestID, "Confirmation approved", "✅ Approved by "+sender, "green")
	log.Infof("confirm.responded workspace=%v chat_id=%v sender_id=%v request_id=%v action=%v tool=%v", ws.ID, chatID, msg.SenderID, requestID, cmd.Action, pc.ToolName)
	r.sendText(chatID, "✅ allowed")
}

func (r *NotificationRouter) handleQuestionCommand(ws *config.Workspace, chatID string, cmd IMCommand, msg IncomingMessage, procKey string, proc *ChordProcess) {
	state := proc.State()
	pq := state.PendingQuestion
	requestID := cmd.RequestID
	if requestID == "" && pq != nil {
		requestID = pq.RequestID
	}
	if requestID == "" || pq == nil {
		expired := r.lookupExpiredPending(procKey)
		q := state.ExpiredQuestion
		if q == nil {
			q = expired.Question
		}
		content := buildExpiredQuestionFollowup(cmd.Answers, q)
		if err := proc.SendUserMessage(content); err != nil {
			log.Errorf("failed to send expired question follow-up workspace=%v error=%v", ws.ID, err)
			r.sendText(chatID, "❌ Failed to send follow-up message to chord.")
			return
		}
		r.beginTurn(procKey)
		r.resolveFeishuCard(msg, procKey, "question", requestID, "Question expired", "⌛ The pending question has expired.", "grey")
		r.sendText(chatID, "⚠️ The pending question has expired. Your response was sent as a follow-up message, not as a structured answer.")
		return
	}
	if requestID != pq.RequestID {
		r.resolveFeishuCard(msg, procKey, "question", requestID, "No matching question", "⚠️ This question has already been handled or no longer matches the current request.", "grey")
		r.sendText(chatID, "⚠️ No matching pending question to answer.")
		return
	}
	answers := resolveQuestionAnswers(strings.Join(cmd.Answers, " "), pq)
	if !r.submitQuestionAnswer(ws, chatID, msg, procKey, proc, pq, requestID, answers, false) {
		return
	}
	r.sendText(chatID, fmt.Sprintf("💬 Answered: %s", strings.Join(answers, ", ")))
}

func (r *NotificationRouter) handleHandoffCommand(ws *config.Workspace, chatID string, cmd IMCommand, procKey string, proc *ChordProcess) {
	if cmd.Invalid {
		r.sendText(chatID, "⚠️ Usage: /handoff <agent> [model_pool] or /handoff-deny <reason>.")
		return
	}
	state := proc.State()
	h := state.PendingHandoff
	if h == nil || strings.TrimSpace(h.RequestID) == "" {
		r.sendText(chatID, "⚠️ No pending handoff to respond to.")
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
		r.sendText(chatID, "❌ Failed to send handoff response.")
		return
	}
	r.beginTurn(procKey)
	if cmd.Action == "deny" {
		if strings.TrimSpace(cmd.Reason) != "" {
			r.sendText(chatID, "✅ handoff denied: "+strings.TrimSpace(cmd.Reason))
			return
		}
		r.sendText(chatID, "✅ handoff denied")
		return
	}
	agentName := strings.TrimSpace(cmd.Agent)
	if agentName == "" {
		agentName = defaultHandoffAgentName(h.Agents)
	}
	if strings.TrimSpace(cmd.Pool) != "" {
		r.sendText(chatID, fmt.Sprintf("✅ handoff accepted: %s (%s)", agentName, strings.TrimSpace(cmd.Pool)))
		return
	}
	r.sendText(chatID, "✅ handoff accepted: "+agentName)
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

func (r *NotificationRouter) handleSendCommand(ws *config.Workspace, chatID string, cmd IMCommand, msg IncomingMessage, procKey string, proc *ChordProcess) {
	// If a pending question exists (and no pending confirm),
	// reinterpret plain text as an answer. This allows the user
	// to simply type their response without /answer prefix.
	// Unlike /answer which supports numeric shortcuts, direct
	// replies are always sent as custom text — no comma splitting
	// or index mapping, because natural language may contain commas.
	state := proc.State()
	if state.PendingConfirm != nil && isDoneTool(state.PendingConfirm.ToolName) && !strings.HasPrefix(strings.TrimSpace(cmd.Content), "/") {
		r.handleConfirmCommand(ws, chatID, IMCommand{Type: "confirm", Action: "deny", Reason: cmd.Content}, msg, procKey, proc)
		return
	}
	if state.PendingQuestion != nil && state.PendingConfirm == nil && !strings.HasPrefix(cmd.Content, "/") {
		pq := state.PendingQuestion
		answers := []string{cmd.Content}
		if !r.submitQuestionAnswer(ws, chatID, msg, procKey, proc, pq, pq.RequestID, answers, true) {
			return
		}
		// Feishu users see the answer reflected on the updated card; avoid duplicating it as a text reply.
		if config.NormalizeIMType(msg.IMType) != "feishu" {
			r.sendText(chatID, fmt.Sprintf("💬 Answered: %s", strings.Join(answers, ", ")))
		}
		return
	}
	// Filter slash commands that are only supported in local TUI.
	// Remote control plane must not forward them.
	switch strings.ToLower(strings.TrimSpace(cmd.Content)) {
	case "/model", "/new", "/resume", "/export":
		r.sendText(chatID, "⚠️ This command is only available in local TUI.")
		return
	}
	if err := proc.SendUserMessage(cmd.Content); err != nil {
		log.Errorf("failed to send user message workspace=%v error=%v", ws.ID, err)
		if r.retrySendInFreshSession(ws, chatID, procKey, cmd.Content) {
			return
		}
		r.sendText(chatID, "❌ Failed to send message to chord.")
		return
	}
	r.beginTurn(procKey)
}

func (r *NotificationRouter) handleLocalShellCommand(ws *config.Workspace, chatID string, cmd IMCommand, procKey string, proc *ChordProcess) {
	command := strings.TrimSpace(cmd.Content)
	if command == "" {
		r.sendText(chatID, "⚠️ Empty command after !")
		return
	}
	if err := proc.SendCommand(map[string]any{"type": "local_shell", "command": command}); err != nil {
		log.Errorf("failed to send local shell command workspace=%v error=%v", ws.ID, err)
		r.sendText(chatID, "❌ Failed to run local shell command.")
		return
	}
	r.beginTurn(procKey)
}

func (r *NotificationRouter) retrySendInFreshSession(ws *config.Workspace, chatID, procKey, content string) bool {
	if r.mgr == nil {
		return false
	}
	if r.mgr.pins != nil {
		if err := r.mgr.clearSessionPinForKey(procKey); err != nil {
			log.Warnf("clear session pin before retry failed key=%v error=%v", procKey, err)
		}
	}
	r.mgr.StopProcessKey(procKey)
	proc, err := r.mgr.SpawnWithArgsForKey(procKey)
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
	r.beginTurn(procKey)
	r.sendText(chatID, "⚠️ Previous Chord session was not found or is busy. Started a new session and sent your message.")
	return true
}

// submitQuestionAnswer forwards an answer to chord, resolves the Feishu card,
// and logs the event. Returns true on success. autoRedirect tags the log line
// to distinguish /answer vs plain-text auto-redirect paths.
func (r *NotificationRouter) submitQuestionAnswer(ws *config.Workspace, chatID string, msg IncomingMessage, procKey string, proc *ChordProcess, pq *QuestionPayload, requestID string, answers []string, autoRedirect bool) bool {
	cmd := map[string]any{
		"type":       "question",
		"request_id": requestID,
		"answers":    answers,
	}
	logSuffix := ""
	if autoRedirect {
		logSuffix = " (auto-redirect)"
	}
	if err := proc.SendCommand(cmd); err != nil {
		log.Errorf("failed to send question response%s workspace=%v error=%v", logSuffix, ws.ID, err)
		r.sendText(chatID, "❌ Failed to send answer.")
		return false
	}
	r.beginTurn(procKey)
	answerText := strings.Join(answers, ", ")
	r.resolveFeishuCard(msg, procKey, "question", requestID, "Question answered", "✅ Answered by "+displaySender(msg)+": "+answerText, "green")
	log.Infof("question.answered workspace=%v chat_id=%v sender_id=%v request_id=%v tool=%v", ws.ID, chatID, msg.SenderID, requestID, pq.ToolName)
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
// role-card button (InternalAction.Value carries the role name, matched by
// exact name and never as a menu number). Every switch
// refetches the role list from chord so menu numbers and the current-role
// check reflect the live configuration instead of a stale menu snapshot.
func (r *NotificationRouter) handleRoleCommand(ws *config.Workspace, chatID string, cmd IMCommand, incoming IncomingMessage, procKey string, proc *ChordProcess) {
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
		r.showRoleMenu(chatID, procKey, current, roles, incoming)
		return
	}

	// A card button carries the exact role name, so it must never take the
	// numeric-menu branch: a role literally named "2" would otherwise resolve
	// to whatever sits at position 2. Plain-text replies keep the
	// number-or-name semantics documented for /role.
	fromCard := incoming.InternalAction != nil && incoming.InternalAction.Type == "role"
	var name string
	if n, atoiErr := strconv.Atoi(target); atoiErr == nil && !fromCard {
		// Numeric menu choice: map against the freshly fetched list.
		if n < 1 || n > len(roles) {
			r.sendText(chatID, "⚠️ Invalid role selection. Send /role to see the current roles.")
			return
		}
		name = strings.TrimSpace(roles[n-1].Name)
	} else {
		name = strings.TrimSpace(target)
		if !roleNameAvailable(name, roles) {
			r.replyRoleResult(chatID, incoming, procKey, "Role unavailable", fmt.Sprintf("⚠️ %q is not an available role. Send /role to see the available roles.", name), "grey")
			return
		}
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
// adapter's sendCardOrFallback has already emitted the text fallback, so the
// handler only falls through to sendText when no Feishu adapter is attached.
func (r *NotificationRouter) showRoleMenu(chatID, procKey, current string, roles []RoleInfo, incoming IncomingMessage) {
	if config.NormalizeIMType(incoming.IMType) == "feishu" {
		if r.sendFeishuRoleMenu(chatID, procKey, current, roles) {
			return
		}
		if r.findFeishuAdapter() != nil {
			return
		}
	}
	r.sendText(chatID, buildRoleMenuText(current, roles))
}

// replyRoleResult reports a /role outcome. A Feishu card click is patched in
// place and does not also send a duplicate chat line; text platforms (and
// Feishu when the card cannot be patched) still get sendText.
func (r *NotificationRouter) replyRoleResult(chatID string, incoming IncomingMessage, procKey, title, message, template string) {
	if r.resolveRoleCard(incoming, procKey, title, message, template) {
		return
	}
	r.sendText(chatID, message)
}

// resolveRoleCard patches the Feishu role menu card when the switch was
// triggered by a card button. It is a no-op for text replies, and reports
// whether a card was actually patched so callers can skip a duplicate chat
// message.
func (r *NotificationRouter) resolveRoleCard(incoming IncomingMessage, procKey, title, message, template string) bool {
	if incoming.InternalAction == nil || incoming.InternalAction.RequestID == "" {
		return false
	}
	return r.resolveFeishuCard(incoming, procKey, "role", incoming.InternalAction.RequestID, title, message, template)
}

// buildRoleMenuText renders the numbered role list used by text platforms and
// as the card fallback.
func buildRoleMenuText(current string, roles []RoleInfo) string {
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
	sb.WriteString("Reply /role <number> or /role <name> to switch.")
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
