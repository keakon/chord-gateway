package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func differentOutboundShardKey(t *testing.T, key string) string {
	t.Helper()
	shard := stableShard(key, outboundDispatchShards)
	for i := 0; i < 1000; i++ {
		candidate := fmt.Sprintf("key-%d", i)
		if stableShard(candidate, outboundDispatchShards) != shard {
			return candidate
		}
	}
	t.Fatal("failed to find key assigned to a different outbound shard")
	return ""
}

func TestOutboundDispatcherPreservesKeyOrder(t *testing.T) {
	d := newOutboundDispatcher()
	var mu sync.Mutex
	var got []int
	for i := 0; i < 3; i++ {
		value := i
		if d.enqueue("same-key", func() {
			mu.Lock()
			got = append(got, value)
			mu.Unlock()
		}) != outboundQueued {
			t.Fatalf("enqueue task %d failed", i)
		}
	}
	d.close()
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(got) != "[0 1 2]" {
		t.Fatalf("task order = %v, want [0 1 2]", got)
	}
}

func TestOutboundDispatcherDifferentShardsProgressIndependently(t *testing.T) {
	d := newOutboundDispatcher()
	slowKey := "slow"
	fastKey := differentOutboundShardKey(t, slowKey)
	releaseSlow := make(chan struct{})
	completed := make(chan string, 2)
	if d.enqueue(slowKey, func() {
		<-releaseSlow
		completed <- "slow"
	}) != outboundQueued {
		t.Fatal("enqueue slow task failed")
	}
	if d.enqueue(fastKey, func() { completed <- "fast" }) != outboundQueued {
		t.Fatal("enqueue fast task failed")
	}
	select {
	case got := <-completed:
		if got != "fast" {
			t.Fatalf("first completed task = %q, want fast", got)
		}
	case <-time.After(time.Second):
		t.Fatal("fast task was blocked by a different shard")
	}
	close(releaseSlow)
	d.close()
	if got := <-completed; got != "slow" {
		t.Fatalf("remaining completed task = %q, want slow", got)
	}
}

func TestOutboundDispatcherCloseDrainsAndRejectsNewTasks(t *testing.T) {
	d := newOutboundDispatcher()
	var completed sync.WaitGroup
	completed.Add(2)
	if got := d.enqueue("key", completed.Done); got != outboundQueued {
		t.Fatalf("first enqueue before close = %v, want queued", got)
	}
	if got := d.enqueue("key", completed.Done); got != outboundQueued {
		t.Fatalf("second enqueue before close = %v, want queued", got)
	}
	d.close()
	completed.Wait()
	snapshot := d.metricsSnapshot()
	if snapshot.Enqueued != 2 || snapshot.Processed != 2 {
		t.Fatalf("metrics after drain = %#v, want 2 enqueued and processed", snapshot)
	}
	if got := d.enqueue("key", func() {}); got != outboundClosed {
		t.Fatalf("enqueue after close = %v, want closed", got)
	}
	// Repeated close must remain safe.
	d.close()
}

func TestOutboundDispatcherReportsFullShard(t *testing.T) {
	d := newOutboundDispatcher()
	key := "blocked"
	release := make(chan struct{})
	started := make(chan struct{})
	if d.enqueue(key, func() {
		close(started)
		<-release
	}) != outboundQueued {
		t.Fatal("enqueue blocking task failed")
	}
	<-started
	for range outboundQueueSize {
		if d.enqueue(key, func() {}) != outboundQueued {
			t.Fatal("queue filled before reaching configured capacity")
		}
	}
	if got := d.enqueue(key, func() {}); got != outboundFull {
		t.Fatalf("enqueue beyond capacity = %v, want full", got)
	}
	if snapshot := d.metricsSnapshot(); snapshot.Full != 1 || snapshot.Enqueued != outboundQueueSize+1 {
		t.Fatalf("metrics at capacity = %#v", snapshot)
	}
	close(release)
	d.close()
}
