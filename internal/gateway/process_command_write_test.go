package gateway

import (
	"errors"
	"io"
	"testing"
)

type shortCommandWriter struct{}

func (shortCommandWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }
func (shortCommandWriter) Close() error                   { return nil }

func TestHeadlessCommandWritesRejectShortWrites(t *testing.T) {
	for _, input := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "input"}[input], func(t *testing.T) {
			p := &ChordProcess{stdin: shortCommandWriter{}}
			var err error
			if input {
				err = p.SendUserMessage("sample")
			} else {
				err = p.SendCommand(map[string]any{"type": "cancel"})
			}
			if !errors.Is(err, io.ErrShortWrite) || len(p.pendingInputs) != 0 {
				t.Fatalf("err=%v pending=%v", err, p.pendingInputs)
			}
		})
	}
}
