package main

import (
	"fmt"
	"testing"
)

// Exercise repeated /status synchronization over the complete durable history.
// The questions themselves are unchanged, as in the common polling path.
func BenchmarkQuestionStatusHistory(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			resp := StatusResponse{SessionID: "session", Questions: make([]*QuestionPayload, count)}
			for i := range resp.Questions {
				resp.Questions[i] = &QuestionPayload{RequestID: fmt.Sprintf("q-%d", i), Version: 2, Outcome: QuestionResolvedAnswered}
			}
			var state ControlState
			state.applyStatusResponse(&resp)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				state.applyStatusResponse(&resp)
			}
		})
	}
}
