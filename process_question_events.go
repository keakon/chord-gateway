package main

import "slices"

type questionEvent struct {
	kind  string
	state ControlState
}

func (p *ChordProcess) questionEvent(kind string, q *QuestionPayload) questionEvent {
	state := p.state
	state.LastQuestion = q
	if q.Outcome != "" {
		state.LastQuestionResolved = q
	}
	return questionEvent{kind: kind, state: state}
}

// State versioning and delivery are separate: a status snapshot may observe a
// committed question before its push reaches stdout. It must not consume the
// later presentation, closure, or revision notification merely by caching it.
// Called with p.mu held.
func (p *ChordProcess) reconcileQuestionEvents(previous map[string]*QuestionPayload) []questionEvent {
	var ids []string
	for id, q := range p.state.Questions {
		if old := previous[id]; q.Outcome == "" || old != nil && old.Outcome == "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var events []questionEvent
	for _, id := range ids {
		q := p.state.Questions[id]
		if q.Outcome == "" {
			// Re-drive presentation after a failed attempt. The router suppresses
			// already delivered prompts and Core deduplicates presented operations.
			events = append(events, p.questionEvent("question_updated", q))
			continue
		}
		if old := previous[id]; old != nil && old.Outcome == "" && p.markQuestionEvent("question_resolved", q) {
			events = append(events, p.questionEvent("question_resolved", q))
		}
	}
	return events
}

func (p *ChordProcess) markQuestionEvent(kind string, q *QuestionPayload) bool {
	if p.questionEventVersions == nil {
		p.questionEventVersions = make(map[string]uint64)
	}
	// A snapshot can close a question at the version of a later revision.
	// Closure must not consume the separate revision notification.
	key := compositeKey(q.RequestID, kind)
	if version, seen := p.questionEventVersions[key]; seen && version >= q.Version {
		return false
	}
	p.questionEventVersions[key] = q.Version
	return true
}
