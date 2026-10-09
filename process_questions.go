package main

import (
	"context"
	"fmt"
)

type QuestionReceipt struct {
	OperationID string `json:"operation_id"`
	RequestID   string `json:"request_id"`
	Accepted    bool   `json:"accepted"`
	Status      string `json:"status"`
	Version     uint64 `json:"version"`
	Error       string `json:"error"`
}

// WaitQuestion registers before writing, and correlates only by operation ID.
// A timeout leaves the outcome unknown; a retry uses the same operation ID.
func (p *ChordProcess) WaitQuestion(ctx context.Context, cmd map[string]any) (QuestionReceipt, error) {
	id, _ := cmd["operation_id"].(string)
	if id == "" {
		return QuestionReceipt{}, fmt.Errorf("question operation ID is required")
	}
	ch := make(chan QuestionReceipt, 1)
	p.mu.Lock()
	if p.questionWaiters == nil {
		p.questionWaiters = map[string]chan QuestionReceipt{}
	}
	if p.questionWaiters[id] != nil {
		p.mu.Unlock()
		return QuestionReceipt{}, fmt.Errorf("question operation is already waiting")
	}
	p.questionWaiters[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.questionWaiters, id); p.mu.Unlock() }()
	if err := p.SendCommand(cmd); err != nil {
		return QuestionReceipt{}, err
	}
	select {
	case receipt := <-ch:
		return receipt, nil
	case <-ctx.Done():
		return QuestionReceipt{}, ctx.Err()
	}
}
