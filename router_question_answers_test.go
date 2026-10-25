package main

import (
	"slices"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

func TestQuestionAnswerIdentityAndCustomText(t *testing.T) {
	q := &QuestionPayload{Multiple: true, Options: []string{"Same label", "Same label", "yes, always"}, OptionIDs: []string{"first", "second", "third"}}
	for _, tc := range []struct {
		name   string
		raw    []string
		action *im.InternalAction
		values []string
		labels []string
		custom bool
	}{
		{name: "numeric duplicate label", raw: []string{"2"}, values: []string{"second"}, labels: []string{"Same label"}},
		{name: "typed ID remains custom", raw: []string{"first"}, values: []string{"first"}, labels: []string{"first"}, custom: true},
		{name: "typed comma remains custom", raw: []string{"yes, please"}, values: []string{"yes, please"}, labels: []string{"yes, please"}, custom: true},
		{name: "out of range remains custom", raw: []string{"4"}, values: []string{"4"}, labels: []string{"4"}, custom: true},
		{name: "mixed indices remain custom", raw: []string{"1,yes"}, values: []string{"1,yes"}, labels: []string{"1,yes"}, custom: true},
		{name: "button uses stable ID", action: &im.InternalAction{Type: "question", Value: "second"}, values: []string{"second"}, labels: []string{"Same label"}},
		{name: "form preserves comma label and order", action: &im.InternalAction{Type: "question", Values: []string{"3", "1"}}, values: []string{"third", "first"}, labels: []string{"yes, always", "Same label"}},
		{name: "invalid form rejects whole answer", action: &im.InternalAction{Type: "question", Values: []string{"1", "4"}}},
		{name: "unknown button rejected", action: &im.InternalAction{Type: "question", Value: "missing"}},
		{name: "empty text rejected", raw: []string{" "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseQuestionAnswer(tc.raw, q, tc.action)
			if !slices.Equal(got.values, tc.values) || !slices.Equal(got.labels, tc.labels) || got.custom != tc.custom {
				t.Fatalf("answer=%+v, want %v %v custom=%v", got, tc.values, tc.labels, tc.custom)
			}
		})
	}
	q.Multiple = false
	if got := parseQuestionAnswer([]string{"1,3"}, q, nil); !got.custom || got.values[0] != "1,3" {
		t.Fatalf("ambiguous single choice=%+v", got)
	}
	if got := parseQuestionAnswer([]string{"1"}, &QuestionPayload{}, nil); !got.custom || got.values[0] != "1" {
		t.Fatalf("free answer=%+v", got)
	}
}
