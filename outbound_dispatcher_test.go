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

func TestOutboundDispatcherEnqueueOrWaitPreservesOrderUnderContention(t *testing.T) {
	d := newOutboundDispatcher()
	key := "ordered"
	releaseFirst := make(chan struct{})
	firstStarted := make(chan struct{})
	completed := make(chan int, outboundQueueSize+3)
	if d.enqueue(key, func() {
		close(firstStarted)
		<-releaseFirst
		completed <- 0
	}) != outboundQueued {
		t.Fatal("enqueue first task failed")
	}
	<-firstStarted
	for i := 1; i <= outboundQueueSize; i++ {
		value := i
		if d.enqueue(key, func() { completed <- value }) != outboundQueued {
			t.Fatalf("enqueue queued task %d failed", i)
		}
	}
	firstQueued := make(chan outboundEnqueueResult, 1)
	go func() {
		firstQueued <- d.enqueueOrWait(key, func() { completed <- outboundQueueSize + 1 })
	}()
	deadline := time.Now().Add(time.Second)
	for d.metricsSnapshot().Full != 1 {
		if time.Now().After(deadline) {
			t.Fatal("first producer did not block on the full shard")
		}
		time.Sleep(time.Millisecond)
	}
	secondQueued := make(chan outboundEnqueueResult, 1)
	go func() {
		secondQueued <- d.enqueueOrWait(key, func() { completed <- outboundQueueSize + 2 })
	}()
	close(releaseFirst)
	if result := <-firstQueued; result != outboundQueued {
		t.Fatalf("first waiting enqueue = %v, want queued", result)
	}
	if result := <-secondQueued; result != outboundQueued {
		t.Fatalf("second waiting enqueue = %v, want queued", result)
	}
	d.close()
	for want := 0; want <= outboundQueueSize+2; want++ {
		if got := <-completed; got != want {
			t.Fatalf("completed task = %d, want %d", got, want)
		}
	}
	if snapshot := d.metricsSnapshot(); snapshot.Full != 1 || snapshot.Blocked != 1 {
		t.Fatalf("overload metrics = %#v, want one full and blocked enqueue", snapshot)
	}
}

func TestOutboundDispatcherCloseWakesBlockingEnqueue(t *testing.T) {
	d := newOutboundDispatcher()
	key := "blocked-close"
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
			t.Fatal("fill queue failed")
		}
	}
	result := make(chan outboundEnqueueResult, 1)
	go func() { result <- d.enqueueOrWait(key, func() {}) }()
	closeDone := make(chan struct{})
	go func() {
		d.close()
		close(closeDone)
	}()
	if got := <-result; got != outboundClosed {
		t.Fatalf("blocking enqueue after close = %v, want closed", got)
	}
	close(release)
	<-closeDone
}
