package main

import (
	"strconv"
	"strings"

	"github.com/keakon/chord-gateway/internal/im"
)

type questionAnswer struct {
	values []string
	labels []string
	custom bool
}

// parseQuestionAnswer resolves identities once. Labels are presentation only;
// custom text remains one answer even when it contains commas or matches an ID.
func parseQuestionAnswer(raw []string, q *QuestionPayload, action *im.InternalAction) questionAnswer {
	if q == nil {
		return questionAnswer{}
	}
	text := strings.TrimSpace(strings.Join(raw, " "))
	custom := func() questionAnswer {
		if text == "" {
			return questionAnswer{}
		}
		return questionAnswer{values: []string{text}, labels: []string{text}, custom: true}
	}
	if action != nil && !action.QuestionFormSubmit() {
		for i, id := range q.OptionIDs {
			if action.Value == id && i < len(q.Options) {
				return questionAnswer{values: []string{id}, labels: []string{q.Options[i]}}
			}
		}
		return questionAnswer{}
	}
	if action == nil && len(q.Options) == 0 {
		return custom()
	}
	parts := strings.Split(text, ",")
	if action != nil {
		parts = action.Values
	}
	if !q.Multiple && len(parts) > 1 {
		if action != nil {
			return questionAnswer{}
		}
		return custom()
	}
	answer := questionAnswer{}
	for _, piece := range parts {
		i, err := strconv.Atoi(strings.TrimSpace(piece))
		if err != nil || i < 1 || i > len(q.OptionIDs) || i > len(q.Options) {
			if action != nil {
				return questionAnswer{}
			}
			return custom()
		}
		answer.values = append(answer.values, q.OptionIDs[i-1])
		answer.labels = append(answer.labels, q.Options[i-1])
	}
	return answer
}
