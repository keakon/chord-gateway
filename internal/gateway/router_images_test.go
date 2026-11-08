package gateway

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keakon/chord-gateway/internal/im"
)

type imageTestAdapter struct {
	stubIMAdapter
	sendImage func(string, *im.ReceivedImage) error
}

func (a *imageTestAdapter) SendImage(chat string, img *im.ReceivedImage) error {
	return a.sendImage(chat, img)
}

func TestImageRouterOrderingFailureAndCleanup(t *testing.T) {
	r := NewNotificationRouter(newTestChordManager(testConfig()))
	var mu sync.Mutex
	var sent []string
	a := &imageTestAdapter{stubIMAdapter: stubIMAdapter{typ: "wechat"}}
	a.sendImage = func(chat string, img *im.ReceivedImage) error {
		mu.Lock()
		sent = append(sent, img.CallID)
		mu.Unlock()
		if chat != "sample-chat" {
			t.Error("wrong chat binding")
		}
		if img.CallID == "second" {
			return fmt.Errorf("upload failed")
		}
		return nil
	}
	r.SetAdapter(a)
	key := processKey{workspaceID: "test", imType: "wechat", chatID: "sample-chat"}.String()
	first, second := imageForUpload(t), imageForUpload(t)
	first.CallID = "first"
	second.CallID = "second"
	im.ImageSpoolBytes.Add(first.Size + second.Size)
	r.HandleChordImage(key, first)
	r.HandleChordImage(key, second)
	r.outbound.close()
	mu.Lock()
	got := append([]string(nil), sent...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("order=%v", got)
	}
	for _, img := range []*im.ReceivedImage{first, second} {
		if _, err := os.Stat(img.Path); !os.IsNotExist(err) {
			t.Fatal("image temp file leaked")
		}
	}
	a.mu.Lock()
	messages := append([]sentMessage(nil), a.sent...)
	a.mu.Unlock()
	if len(messages) != 1 || messages[0].chatID != "sample-chat" {
		t.Fatal("delivery failure did not reach source chat")
	}
	rejected := imageForUpload(t)
	im.ImageSpoolBytes.Add(rejected.Size)
	r.HandleChordImage(key, rejected)
	if _, err := os.Stat(rejected.Path); !os.IsNotExist(err) {
		t.Fatal("closed queue leaked image")
	}
}

// An image enqueue that cannot get shard capacity must give up within the
// outbound wait cap instead of stalling the chord stdout reader for the rest
// of the binding lifetime.
func TestImageRouterEnqueueWaitCapDropsImage(t *testing.T) {
	oldCap := outboundEnqueueWaitCap
	outboundEnqueueWaitCap = 50 * time.Millisecond
	t.Cleanup(func() { outboundEnqueueWaitCap = oldCap })

	r := NewNotificationRouter(newTestChordManager(testConfig()))
	defer r.outbound.close()
	key := processKey{workspaceID: "test", imType: "wechat", chatID: "sample-chat"}.String()
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 1)
	fill := func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	// One blocking task (picked up by the consumer) plus a full queue leaves
	// the shard saturated, so the image enqueue has to wait.
	if got := r.outbound.enqueue(key, fill); got != outboundQueued {
		t.Fatalf("first enqueue = %v, want outboundQueued", got)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first task did not start")
	}
	for i := range outboundQueueSize {
		if got := r.outbound.enqueue(key, fill); got != outboundQueued {
			t.Fatalf("fill enqueue %d = %v, want outboundQueued", i, got)
		}
	}

	img := imageForUpload(t)
	initial := im.ImageSpoolBytes.Load()
	im.ImageSpoolBytes.Add(img.Size)
	done := make(chan struct{})
	go func() { r.HandleChordImage(key, img); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("image enqueue did not respect the wait cap")
	}
	if _, err := os.Stat(img.Path); !os.IsNotExist(err) {
		t.Fatal("dropped image leaked its temp file")
	}
	if im.ImageSpoolBytes.Load() != initial {
		t.Fatal("dropped image leaked spool budget")
	}
}

func TestImageRouterMarksOnlyConfirmedVisibleOutput(t *testing.T) {
	for _, tc := range []struct {
		name                string
		imageErr, noticeErr error
		visible             bool
		text                string
	}{
		{name: "image", visible: true},
		{name: "failure_notice", imageErr: errors.New("upload rejected"), visible: true, text: "Image delivery failed"},
		{name: "unknown_notice", imageErr: fmt.Errorf("send image: %w", im.ErrDeliveryOutcomeUnknown), visible: true, text: "Image delivery is unconfirmed"},
		{name: "undelivered_notice", imageErr: im.ErrDeliveryOutcomeUnknown, noticeErr: errors.New("text rejected"), visible: false, text: "Image delivery is unconfirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newTestChordManager(testConfig())
			key := processKey{workspaceID: "test", imType: "wechat", chatID: "sample-chat"}.String()
			before := time.Now().Add(-time.Minute)
			proc := &ChordProcess{state: ControlState{Busy: true, LastPushAt: before, InternalEventsSinceLastPush: 5}}
			mgr.procs[key] = proc
			r := NewNotificationRouter(mgr)
			defer r.outbound.close()
			sends := 0
			a := &imageTestAdapter{stubIMAdapter: stubIMAdapter{typ: "wechat", sendFunc: func(_, _ string) error { return tc.noticeErr }}, sendImage: func(_ string, _ *im.ReceivedImage) error { sends++; return tc.imageErr }}
			r.SetAdapter(a)
			img := imageForUpload(t)
			initial := im.ImageSpoolBytes.Load()
			im.ImageSpoolBytes.Add(img.Size)
			r.HandleChordImage(key, img)
			r.outbound.close()
			state := proc.State()
			if tc.visible {
				if !state.LastPushAt.After(before) || state.InternalEventsSinceLastPush != 0 {
					t.Fatal("visible output did not reset reminder state")
				}
			} else if !state.LastPushAt.Equal(before) || state.InternalEventsSinceLastPush != 5 {
				t.Fatal("failed output reset reminder state")
			}
			if sends != 1 || im.ImageSpoolBytes.Load() != initial {
				t.Fatal("send replay or image budget leak")
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if tc.imageErr == nil {
				if len(a.sent) != 0 {
					t.Fatal("successful image reported as failure")
				}
			} else if len(a.sent) != 1 || !strings.Contains(a.sent[0].text, tc.text) {
				t.Fatalf("notice = %v", a.sent)
			}
		})
	}
}
