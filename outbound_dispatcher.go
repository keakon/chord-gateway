package main

import (
	"context"
	"sync"
)

const (
	outboundDispatchShards = 8
	outboundQueueSize      = 256
)

type outboundTask func()

type outboundEnqueueResult uint8

const (
	outboundClosed outboundEnqueueResult = iota
	outboundQueued
	outboundFull
)

type outboundDispatcher struct {
	mu      sync.RWMutex
	closed  bool
	cancel  context.CancelFunc
	queues  [outboundDispatchShards]chan outboundTask
	metrics queueMetrics
	wg      sync.WaitGroup
}

func newOutboundDispatcher() *outboundDispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	d := &outboundDispatcher{cancel: cancel}
	d.wg.Add(len(d.queues))
	for i := range d.queues {
		d.queues[i] = make(chan outboundTask, outboundQueueSize)
		go d.consume(ctx, d.queues[i])
	}
	return d
}

func (d *outboundDispatcher) enqueue(key string, task outboundTask) outboundEnqueueResult {
	if d == nil || task == nil {
		return outboundClosed
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		d.metrics.closed.Add(1)
		return outboundClosed
	}
	select {
	case d.queues[stableShard(key, len(d.queues))] <- task:
		d.metrics.enqueued.Add(1)
		return outboundQueued
	default:
		d.metrics.full.Add(1)
		return outboundFull
	}
}

func (d *outboundDispatcher) close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.cancel()
	d.mu.Unlock()
	d.wg.Wait()
}

func (d *outboundDispatcher) consume(ctx context.Context, queue <-chan outboundTask) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case task := <-queue:
					task()
					d.metrics.processed.Add(1)
				default:
					return
				}
			}
		case task := <-queue:
			task()
			d.metrics.processed.Add(1)
		}
	}
}

func (d *outboundDispatcher) metricsSnapshot() queueMetricsSnapshot {
	if d == nil {
		return queueMetricsSnapshot{}
	}
	depth := 0
	capacity := 0
	for _, queue := range d.queues {
		depth += len(queue)
		capacity += cap(queue)
	}
	return d.metrics.snapshot(depth, capacity)
}

func stableShard(key string, count int) int {
	const offset64 = uint64(14695981039346656037)
	const prime64 = uint64(1099511628211)
	hash := offset64
	for i := 0; i < len(key); i++ {
		hash ^= uint64(key[i])
		hash *= prime64
	}
	return int(hash % uint64(count))
}
