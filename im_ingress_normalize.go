package main

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/keakon/chord-gateway/internal/im"
)

const (
	ingressNormalizationWorkers  = 8
	ingressNormalizationCapacity = 128
)

// CaptureIngressEnvelope freezes controls on the transport reader before slow
// identity lookups. The resulting message still requires normal admission.
func (r *NotificationRouter) CaptureIngressEnvelope(e im.IngressEnvelope) im.IngressEnvelope {
	// Callback decoders may reuse their buffers after the reader returns. Values
	// and the action must remain the operation observed at receive time.
	e.Cursor = im.CloneIngressCursor(e.Cursor)
	if e.Message.InternalAction != nil {
		action := *e.Message.InternalAction
		action.Values = slices.Clone(action.Values)
		e.Message.InternalAction = &action
	}
	if e.Capture != nil {
		return e
	}
	kind, target, ok := r.tryCaptureIngress(e.Message)
	e.Capture = &im.IngressCapture{Kind: kind, Target: target}
	if !ok {
		e.Capture.Err = im.ErrIngressCaptureBusy
	}
	return e
}

type ingressNormalization struct {
	ctx       context.Context
	normalize func(context.Context) (im.IngressEnvelope, error)
	protocol  func(context.Context) im.ReceiveResult
	result    chan im.ReceiveResult
	observe   func(im.ReceiveResult)
}

// IngressNormalizer separates socket readers from network lookups and admission.
// One instance is shared by all gateway accounts. Submit must be called in
// transport receive order, using an account-scoped raw channel key. A thread
// may share its parent channel until lookup finishes. Normalize and Accept must
// respect context cancellation so shutdown can join the workers.
type IngressNormalizer struct {
	ctx      context.Context
	cancel   context.CancelFunc
	receiver im.IngressReceiver
	mu       sync.Mutex
	queues   map[string][]ingressNormalization
	ready    chan string
	count    int
	closed   bool
	wg       sync.WaitGroup
	done     chan struct{}
}

func NewIngressNormalizer(ctx context.Context, receiver im.IngressReceiver) *IngressNormalizer {
	ctx, cancel := context.WithCancel(ctx)
	n := &IngressNormalizer{ctx: ctx, cancel: cancel, receiver: receiver, queues: make(map[string][]ingressNormalization), ready: make(chan string, ingressNormalizationCapacity), done: make(chan struct{})}
	for range ingressNormalizationWorkers {
		n.wg.Go(n.worker)
	}
	// Parent cancellation closes queued result channels even if the owner has
	// not yet called Close. Only this goroutine drains after all workers exit.
	go n.shutdown()
	return n
}

// Submit never blocks the reader or a heartbeat. A full queue is uncommitted;
// the transport decides whether to withhold ACK or explicitly reject.
func (n *IngressNormalizer) Submit(key string, normalize func(context.Context) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
	return n.SubmitContext(n.ctx, key, normalize)
}

// SubmitContext cancels an individual lookup without cancelling other routes.
// Already-running admission retains its actual result, including uncertainty.
func (n *IngressNormalizer) SubmitContext(ctx context.Context, key string, normalize func(context.Context) (im.IngressEnvelope, error)) (<-chan im.ReceiveResult, error) {
	return n.submitObserved(ctx, key, normalize, nil)
}

// submitObserved reports the authoritative outcome on an existing worker. The
// observer must not block or perform network I/O. It also receives the definite
// cancellation of queued work during shutdown, without an unbounded waiter.
func (n *IngressNormalizer) submitObserved(ctx context.Context, key string, normalize func(context.Context) (im.IngressEnvelope, error), observe func(im.ReceiveResult)) (<-chan im.ReceiveResult, error) {
	return n.submit(ctx, key, normalize, nil, observe)
}

// submitProtocol shares the Gateway's bounded worker budget for identity probes
// and permanent transport refusals. These operations never dispatch business input.
func (n *IngressNormalizer) submitProtocol(ctx context.Context, key string, protocol func(context.Context) im.ReceiveResult) (<-chan im.ReceiveResult, error) {
	return n.submit(ctx, key, nil, protocol, nil)
}

func (n *IngressNormalizer) submit(ctx context.Context, key string, normalize func(context.Context) (im.IngressEnvelope, error), protocol func(context.Context) im.ReceiveResult, observe func(im.ReceiveResult)) (<-chan im.ReceiveResult, error) {
	n.mu.Lock()
	if n.closed || n.ctx.Err() != nil {
		n.mu.Unlock()
		return nil, im.ErrIngressClosed
	}
	if err := ctx.Err(); err != nil {
		n.mu.Unlock()
		return nil, err
	}
	if key == "" || (normalize == nil && protocol == nil) || n.receiver == nil {
		n.mu.Unlock()
		return nil, errors.New("invalid ingress normalization")
	}
	if n.count >= ingressNormalizationCapacity {
		n.mu.Unlock()
		return nil, im.ErrIngressCapacity
	}
	result := make(chan im.ReceiveResult, 1)
	_, exists := n.queues[key]
	n.queues[key] = append(n.queues[key], ingressNormalization{ctx: ctx, normalize: normalize, protocol: protocol, result: result, observe: observe})
	n.count++
	n.mu.Unlock()
	if !exists {
		// Send outside the mutex: ready is buffered for every queued key,
		// so this never blocks a transport reader on lock ordering.
		n.ready <- key
	}
	return result, nil
}

func (n *IngressNormalizer) worker() {
	for {
		select {
		case <-n.ctx.Done():
			return
		case key := <-n.ready:
			n.mu.Lock()
			queue := n.queues[key]
			if len(queue) == 0 {
				n.mu.Unlock()
				continue
			}
			item := queue[0]
			n.mu.Unlock()
			result := n.normalize(item)
			item.result <- result
			close(item.result)
			if item.observe != nil {
				item.observe(result)
			}
			n.mu.Lock()
			n.queues[key][0] = ingressNormalization{}
			n.queues[key] = n.queues[key][1:]
			n.count--
			requeue := len(n.queues[key]) != 0
			if !requeue {
				delete(n.queues, key)
			}
			n.mu.Unlock()
			if requeue {
				// The global capacity includes running items, so a successor
				// always has space. Tail requeue gives other keys a turn.
				n.ready <- key
			}
		}
	}
}

func (n *IngressNormalizer) normalize(item ingressNormalization) im.ReceiveResult {
	ctx, cancel := context.WithCancel(item.ctx)
	stop := context.AfterFunc(n.ctx, cancel)
	defer stop()
	defer cancel()
	if n.ctx.Err() != nil {
		cancel()
	}
	cancelled := im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "normalization_cancelled"}
	if ctx.Err() != nil {
		return cancelled
	}
	if item.protocol != nil {
		// Once a protocol transaction begins its actual result is authoritative,
		// just as it is for ordinary in-memory admission.
		return item.protocol(ctx)
	}
	envelope, err := item.normalize(ctx)
	if ctx.Err() != nil {
		return cancelled
	}
	if err != nil {
		return im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "normalization_failed"}
	}
	// Once admission begins, its result is authoritative. Cancelling ctx must
	// not overwrite a possible in-memory accepted/uncertain result.
	return n.receiver.Accept(ctx, envelope)
}

func (n *IngressNormalizer) shutdown() {
	<-n.ctx.Done()
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()
	n.wg.Wait()
	n.mu.Lock()
	queues := n.queues
	n.queues = make(map[string][]ingressNormalization)
	n.count = 0
	n.mu.Unlock()
	for _, queue := range queues {
		for _, item := range queue {
			result := im.ReceiveResult{Disposition: im.ReceiveRetryable, Reason: "normalization_cancelled"}
			item.result <- result
			close(item.result)
			if item.observe != nil {
				item.observe(result)
			}
		}
	}
	close(n.done)
}

// IngressNormalizationSnapshot exposes bounded queue use without raw chat IDs.
type IngressNormalizationSnapshot struct {
	Pending int
	Keys    int
	Closed  bool
}

func (n *IngressNormalizer) Snapshot() IngressNormalizationSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return IngressNormalizationSnapshot{Pending: n.count, Keys: len(n.queues), Closed: n.closed || n.ctx.Err() != nil}
}

func (n *IngressNormalizer) Close() {
	n.cancel()
	<-n.done
}
