package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

const (
	maxQuestionOperations = 128
	questionOperationTTL  = 14 * 24 * time.Hour
)

// Only uncertain operations are retained. An explicit retry of the same action
// by the same user reuses the complete original command, including answer vs
// revise, even if a status snapshot has since observed the terminal outcome.
type pendingQuestionOperation struct {
	command   map[string]any
	expiresAt time.Time
	recordIDs []string
	target    *im.IngressControlTarget
}

func (r *NotificationRouter) waitQuestionOperation(ctx context.Context, key, sender string, proc *ChordProcess, command map[string]any) (QuestionReceipt, error) {
	return r.waitQuestionOperationTarget(ctx, key, sender, proc, command, nil, nil)
}

func questionOperationIdentity(key, sender string, command map[string]any, target *im.IngressControlTarget) (string, error) {
	identity := maps.Clone(command)
	delete(identity, "operation_id")
	delete(identity, "version")
	if target != nil {
		key = compositeKey(key, target.RuntimeID, fmt.Sprint(target.SessionEpoch), target.SessionID, target.BindingInstanceID)
	}
	if identity["action"] == "answer" || identity["action"] == "revise" {
		identity["action"] = "answer_or_revise"
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	return compositeKey(key, sender, string(data)), nil
}

func (r *NotificationRouter) waitQuestionOperationTarget(ctx context.Context, key, sender string, proc *ChordProcess, command map[string]any, target *im.IngressControlTarget, recordIDs []string) (QuestionReceipt, error) {
	operationKey, err := questionOperationIdentity(key, sender, command, target)
	if err != nil {
		return QuestionReceipt{}, err
	}
	now := time.Now()
	r.mu.Lock()
	if r.questionOperations == nil {
		r.questionOperations = make(map[string]pendingQuestionOperation)
	}
	for id, op := range r.questionOperations {
		if !now.Before(op.expiresAt) {
			delete(r.questionOperations, id)
		}
	}
	if previous, ok := r.questionOperations[operationKey]; ok {
		command = previous.command
		previous.recordIDs = append(append([]string(nil), previous.recordIDs...), recordIDs...)
		r.questionOperations[operationKey] = previous
	} else {
		if len(r.questionOperations) >= maxQuestionOperations {
			r.mu.Unlock()
			_, platform, chat := parseProcessKey(key)
			return QuestionReceipt{}, fmt.Errorf("too many unconfirmed question operations; check %s before retrying", r.commandHint(platform, im.CommandTargetForChat(platform, chat), "/status"))
		}
		command = maps.Clone(command)
		r.questionOperations[operationKey] = pendingQuestionOperation{command: command, expiresAt: now.Add(questionOperationTTL), recordIDs: recordIDs, target: target}
	}
	r.mu.Unlock()
	var receipt QuestionReceipt
	if target == nil {
		receipt, err = proc.WaitQuestion(ctx, command)
	} else {
		proc.mu.Lock()
		proc.onQuestionReceipt = r.enqueueIngressQuestionReceipt
		proc.mu.Unlock()
		receipt, err = proc.waitIngressQuestion(ctx, command, target)
	}
	if err == nil {
		if target != nil {
			if err := r.ObserveIngressQuestionReceipt(proc, receipt); err != nil {
				return QuestionReceipt{}, err
			}
			return receipt, nil
		}
		r.mu.Lock()
		if current := r.questionOperations[operationKey]; current.command["operation_id"] == command["operation_id"] {
			delete(r.questionOperations, operationKey)
		}
		r.mu.Unlock()
	}
	return receipt, err
}
