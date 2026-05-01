package main

import (
	"context"
	"fmt"
	"github.com/keakon/golog/log"
	"strings"
	"time"

	"github.com/keakon/chord-gateway/config"
)

// handleChordCommand dispatches a command to the chord process.
func (r *NotificationRouter) handleChordCommand(ws *config.Workspace, chatID string, cmd IMCommand, imType string, messages ...IncomingMessage) {
	msg := IncomingMessage{IMType: imType, ChatID: chatID, SenderID: chatID}
	if len(messages) > 0 {
		msg = messages[0]
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
		r.handleConfirmCommand(ws, chatID, cmd, msg, procKey, proc)
	case "question":
		r.handleQuestionCommand(ws, chatID, cmd, msg, procKey, proc)
	case "send":
		r.handleSendCommand(ws, chatID, cmd, msg, procKey, proc)
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
		r.updateFeishuCardStatus(msg, procKey, "confirm", cmd.RequestID, buildFeishuResolvedCard("Confirmation expired", "⌛ The pending confirmation has expired.", "grey"))
		r.sendText(chatID, "⚠️ The pending confirmation has expired. Your response was sent as a follow-up message, not as an approval or denial.")
		return
	}
	requestID := pc.RequestID
	if cmd.RequestID != "" {
		if cmd.RequestID != pc.RequestID {
			r.updateFeishuCardStatus(msg, procKey, "confirm", cmd.RequestID, buildFeishuResolvedCard("No matching confirmation", "⚠️ This confirmation has already been handled or no longer matches the current request.", "grey"))
			r.sendText(chatID, "⚠️ No matching pending confirmation to respond to.")
			return
		}
		requestID = cmd.RequestID
	}
	confirmCmd := map[string]any{
		"type":       "confirm",
		"request_id": requestID,
		"action":     cmd.Action,
	}
	if cmd.Action == "deny" && strings.TrimSpace(cmd.Reason) != "" {
		confirmCmd["deny_reason"] = strings.TrimSpace(cmd.Reason)
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
		if strings.TrimSpace(cmd.Reason) != "" {
			text += ": " + strings.TrimSpace(cmd.Reason)
			status += ": " + strings.TrimSpace(cmd.Reason)
		}
		r.updateFeishuCardStatus(msg, procKey, "confirm", requestID, buildFeishuResolvedCard("Confirmation denied", status, "red"))
		log.Infof("confirm.responded workspace=%v chat_id=%v sender_id=%v request_id=%v action=%v tool=%v", ws.ID, chatID, msg.SenderID, requestID, cmd.Action, pc.ToolName)
		r.sendText(chatID, text)
		return
	}
	r.updateFeishuCardStatus(msg, procKey, "confirm", requestID, buildFeishuResolvedCard("Confirmation approved", "✅ Approved by "+sender, "green"))
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
		r.updateFeishuCardStatus(msg, procKey, "question", requestID, buildFeishuResolvedCard("Question expired", "⌛ The pending question has expired.", "grey"))
		r.sendText(chatID, "⚠️ The pending question has expired. Your response was sent as a follow-up message, not as a structured answer.")
		return
	}
	if requestID != pq.RequestID {
		r.updateFeishuCardStatus(msg, procKey, "question", requestID, buildFeishuResolvedCard("No matching question", "⚠️ This question has already been handled or no longer matches the current request.", "grey"))
		r.sendText(chatID, "⚠️ No matching pending question to answer.")
		return
	}
	answers := resolveQuestionAnswers(strings.Join(cmd.Answers, " "), pq)
	questionCmd := map[string]any{
		"type":       "question",
		"request_id": requestID,
		"answers":    answers,
	}
	if err := proc.SendCommand(questionCmd); err != nil {
		log.Errorf("failed to send question response workspace=%v error=%v", ws.ID, err)
		r.sendText(chatID, "❌ Failed to send answer.")
		return
	}
	r.beginTurn(procKey)
	answerText := strings.Join(answers, ", ")
	r.updateFeishuCardStatus(msg, procKey, "question", requestID, buildFeishuResolvedCard("Question answered", "✅ Answered by "+displaySender(msg)+": "+answerText, "green"))
	log.Infof("question.answered workspace=%v chat_id=%v sender_id=%v request_id=%v tool=%v", ws.ID, chatID, msg.SenderID, requestID, pq.ToolName)
	r.sendText(chatID, fmt.Sprintf("💬 Answered: %s", answerText))
}

func (r *NotificationRouter) handleSendCommand(ws *config.Workspace, chatID string, cmd IMCommand, msg IncomingMessage, procKey string, proc *ChordProcess) {
	// If a pending question exists (and no pending confirm),
	// reinterpret plain text as an answer. This allows the user
	// to simply type their response without /answer prefix.
	// Unlike /answer which supports numeric shortcuts, direct
	// replies are always sent as custom text — no comma splitting
	// or index mapping, because natural language may contain commas.
	if proc.State().PendingQuestion != nil && proc.State().PendingConfirm == nil {
		if !strings.HasPrefix(cmd.Content, "/") {
			pq := proc.State().PendingQuestion
			answers := []string{cmd.Content}
			requestID := pq.RequestID
			questionCmd := map[string]any{
				"type":       "question",
				"request_id": requestID,
				"answers":    answers,
			}
			if err := proc.SendCommand(questionCmd); err != nil {
				log.Errorf("failed to send question response (auto-redirect) workspace=%v error=%v", ws.ID, err)
				r.sendText(chatID, "❌ Failed to send answer.")
				return
			}
			r.beginTurn(procKey)
			answerText := strings.Join(answers, ", ")
			r.updateFeishuCardStatus(msg, procKey, "question", requestID, buildFeishuResolvedCard("Question answered", "✅ Answered by "+displaySender(msg)+": "+answerText, "green"))
			log.Infof("question.answered workspace=%v chat_id=%v sender_id=%v request_id=%v tool=%v", ws.ID, chatID, msg.SenderID, requestID, pq.ToolName)
			// Feishu users see the answer reflected on the updated card; avoid duplicating it as a text reply.
			if normalizeIMType(msg.IMType) != "feishu" {
				r.sendText(chatID, fmt.Sprintf("💬 Answered: %s", answerText))
			}
			return
		}
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
		r.sendText(chatID, "❌ Failed to send message to chord.")
		return
	}
	r.beginTurn(procKey)
}
