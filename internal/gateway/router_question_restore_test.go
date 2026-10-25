package gateway

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

type questionSnapshotWriter struct {
	captureWriteCloser
	proc      *ChordProcess
	snapshot  StatusResponse
	statusErr error
}

func (w *questionSnapshotWriter) Write(data []byte) (int, error) {
	var cmd map[string]any
	if err := json.Unmarshal(data, &cmd); err != nil {
		return 0, err
	}
	if cmd["type"] == "status" && w.statusErr != nil {
		return 0, w.statusErr
	}
	n, err := w.captureWriteCloser.Write(data)
	if cmd["type"] == "status" {
		payload, _ := json.Marshal(w.snapshot)
		go w.proc.processEnvelope(&HeadlessEnvelope{Type: "status_response", Seq: 1, Payload: payload})
	} else if cmd["type"] == "question" {
		payload, _ := json.Marshal(QuestionReceipt{OperationID: cmd["operation_id"].(string), Accepted: true})
		go w.proc.processEnvelope(&HeadlessEnvelope{Type: "question_receipt", Payload: payload})
	}
	return n, err
}

func TestHistoricalAnswerSynchronizesBeforeSubmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		found     bool
		statusErr error
		want      string
	}{
		{name: "restored history", found: true, want: "Answer accepted (old)"},
		{name: "unknown ID", want: "old was not found"},
		{name: "sync failure", statusErr: io.ErrClosedPipe, want: "Could not synchronize"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &ChordProcess{}
			w := &questionSnapshotWriter{proc: p, statusErr: tc.statusErr, snapshot: StatusResponse{SessionID: "restored-session"}}
			if tc.found {
				w.snapshot.Questions = []*QuestionPayload{{RequestID: "old", Version: 3, Outcome: QuestionResolvedAnswered}}
			}
			p.stdin = w
			sender := &stubIMAdapter{typ: "wechat"}
			r := &NotificationRouter{adapter: sender}
			r.handleQuestionCommand(&config.Workspace{ID: "workspace"}, "chat", im.IMCommand{Type: "question", RequestID: "old", Answers: []string{"Revised"}}, im.IncomingMessage{IMType: "wechat", ChatID: "chat", MessageID: "reply"}, "binding", p)
			if !strings.Contains(sender.lastMessage().text, tc.want) {
				t.Fatalf("response=%q", sender.lastMessage().text)
			}
			if tc.found && (!strings.Contains(w.String(), `"question_ids":["old"],"type":"status"`) || !strings.Contains(w.String(), `"action":"revise"`)) {
				t.Fatalf("restored answer commands=%s", w.String())
			}
			if !tc.found && strings.Contains(w.String(), `"type":"question"`) {
				t.Fatal("submitted without a synchronized question")
			}
		})
	}
}
