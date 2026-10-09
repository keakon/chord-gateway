package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"
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
}

func (r *NotificationRouter) waitQuestionOperation(ctx context.Context, key, sender string, proc *ChordProcess, command map[string]any) (QuestionReceipt, error) {
	identity := maps.Clone(command)
	delete(identity, "operation_id")
	if identity["action"] == "answer" || identity["action"] == "revise" {
		identity["action"] = "answer_or_revise"
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return QuestionReceipt{}, err
	}
	operationKey := compositeKey(key, sender, string(data))
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
	} else {
		if len(r.questionOperations) >= maxQuestionOperations {
			r.mu.Unlock()
			return QuestionReceipt{}, fmt.Errorf("too many unconfirmed question operations; check /status before retrying")
		}
		command = maps.Clone(command)
		r.questionOperations[operationKey] = pendingQuestionOperation{command: command, expiresAt: now.Add(questionOperationTTL)}
	}
	r.mu.Unlock()
	receipt, err := proc.WaitQuestion(ctx, command)
	if err == nil {
		r.mu.Lock()
		if current := r.questionOperations[operationKey]; current.command["operation_id"] == command["operation_id"] {
			delete(r.questionOperations, operationKey)
		}
		r.mu.Unlock()
	}
	return receipt, err
}
