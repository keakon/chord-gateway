package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/buildinfo"
	"github.com/keakon/chord-gateway/internal/im"
)

// ChordProcess manages a single chord headless child process.
type ChordProcess struct {
	// Protected by mgr.mu; independent of process exit so final output can drain.
	deliveryCtx    context.Context
	deliveryCancel context.CancelFunc

	lease *im.BindingLease
	gate  *im.ChatGate

	waitDone chan struct{}

	bindingInstanceID   string
	runtimeID           string
	sessionEpoch        uint64
	workspacePath       string
	transfer            *TransferStatus
	restoringTransferID string
	bindingGuard        bool
	expectedSessionID   string
	readyPaused         bool
	readySignal         chan struct{}
	readyOnce           sync.Once
	images              imageReceiver
	mu                  sync.RWMutex
	cmd                 *exec.Cmd
	stdin               io.WriteCloser
	cancel              context.CancelFunc

	waitOnce sync.Once
	exitOnce sync.Once
	exited   bool
	exitCode int
	stderr   *tailBuffer

	pgid int

	key          string
	workspaceID  string
	state        ControlState
	lastActivity time.Time

	// compactionPlanID tracks the plan id of the compaction that currently
	// owns the single compaction slot (set by a non-synthetic started, cleared
	// by its terminal). Guards state.LastCompaction from being overwritten by
	// synthetic sync-skip pairs or superseded plans' late outcomes.
	compactionPlanID      string
	pendingInputs         map[string]struct{}
	inputObservers        map[string]*processInputObserver
	inputReceiptQueue     []string
	inputReceiptWorker    bool
	inputReceiptBlocked   bool
	inputReceiptShutdown  bool
	inputReceiptDone      chan struct{}
	questionEventVersions map[string]uint64
	lastMCPNotification   string
	// mcpNotified records whether this process has pushed any MCP
	// notification. The all-available summary is only pushed as a recovery
	// after a problem was reported; see process_protocol.go.
	mcpNotified bool

	// Auto-restart on crash
	autoRestart      bool
	stoppedByGateway bool
	mgr              *ChordManager // back-reference for auto-restart

	// Callback for events that need IM notification.
	onEvent   func(key string, eventType string, state ControlState)
	eventLogf func(format string, args ...any)

	// statusWaiters are notified when a status_response envelope arrives.
	statusWaiters envelopeWaiters[ControlState]
	// roleWaiters are notified when a role_response envelope arrives.
	roleWaiters envelopeWaiters[RoleResponse]
	// roleMu serializes WaitRoleList/WaitRoleSwitch. Role responses carry no
	// request id, and roleWaiters wakes every waiter with the same payload, so
	// two concurrent waits could consume each other's response. It is held only
	// for the Chord round-trip, not for IM card/text I/O.
	roleMu            sync.Mutex
	questionWaiters   map[string]chan QuestionReceipt
	onQuestionReceipt func(*ChordProcess, QuestionReceipt) bool
}

var ErrManagerShuttingDown = errors.New("chord manager is shutting down")

// tailBuffer keeps the last N bytes written.
// Used to capture child-process stderr without unbounded memory growth.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newTailBuffer(capacity int) *tailBuffer {
	if capacity <= 0 {
		capacity = 32 * 1024
	}
	return &tailBuffer{cap: capacity}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.cap {
		// Keep only the tail.
		t.buf = append([]byte(nil), p[len(p)-t.cap:]...)
		return len(p), nil
	}
	need := len(t.buf) + len(p) - t.cap
	if need > 0 {
		t.buf = t.buf[need:]
	}
	t.buf = append(t.buf, p...)
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ChordManager manages chord headless processes, one per workspace.
type ChordManager struct {
	inputReceiptProcesses sync.Map // process pointers with durable receipt observers
	admission             *Admission

	store *ConfigStore

	onImage       func(string, *im.ReceivedImage)
	mu            sync.RWMutex
	lifecycleGate sync.RWMutex
	shuttingDown  atomic.Bool
	shutdownOnce  sync.Once
	shutdownCh    chan struct{}
	keyLocksMu    sync.Mutex
	keyLocks      map[string]*lifecycleKeyLock
	cfg           atomic.Pointer[config.Config]
	procs         map[string]*ChordProcess // processKey.String() → active process
	onEvent       func(key string, eventType string, state ControlState)

	pins                *sessionPinStore
	transfers           *sessionTransferStore
	takeoverCLI         func(context.Context, string, takeoverRequest) (TransferStatus, error)
	capabilityMu        sync.Mutex
	managedCapabilities map[string]bool
}

type lifecycleKeyLock struct {
	mu   sync.Mutex
	refs int
}

func (m *ChordManager) acquireKeyLock(key string) func() {
	m.keyLocksMu.Lock()
	if m.keyLocks == nil {
		m.keyLocks = make(map[string]*lifecycleKeyLock)
	}
	lock := m.keyLocks[key]
	if lock == nil {
		lock = &lifecycleKeyLock{}
		m.keyLocks[key] = lock
	}
	lock.refs++
	m.keyLocksMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		m.keyLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(m.keyLocks, key)
		}
		m.keyLocksMu.Unlock()
	}
}

// NewChordManager creates a new ChordManager.
func NewChordManager(cfg *config.Config, paths *config.Paths) *ChordManager {
	pinsPath := ""
	if cfg != nil {
		pinsPath = strings.TrimSpace(cfg.SessionPinsFile)
	}

	pins := newSessionPinStore(paths.StateDir)
	if pinsPath != "" {
		pins.path = pinsPath
	}
	transfers := newSessionTransferStore(paths.StateDir)
	if err := transfers.Load(); err != nil {
		log.Errorf("load transfer guards failed: %v", err)
	}
	_ = pins.Load()

	m := &ChordManager{
		procs:      make(map[string]*ChordProcess),
		pins:       pins,
		transfers:  transfers,
		shutdownCh: make(chan struct{}),
	}
	m.cfg.Store(cfg)
	return m
}

// Config returns the current config snapshot.
func (m *ChordManager) Config() *config.Config {
	if m.store != nil {
		return m.store.Snapshot()
	}
	return m.cfg.Load()
}

// SetOnEvent registers a callback invoked when a chord process emits a notable event.
func (m *ChordManager) SetOnEvent(fn func(key, eventType string, state ControlState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEvent = fn
}

func (m *ChordManager) SetOnImage(fn func(string, *im.ReceivedImage)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onImage = fn
}

// GetProcessForKey returns the active process for a process key, or nil if none exists.
func (m *ChordManager) GetProcessForKey(key string) *ChordProcess {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.procs[key]
}

func (m *ChordManager) GetOrSpawnForKey(key string) (*ChordProcess, error) {
	release, err := m.acquireChatOperation(key)
	if err != nil {
		return nil, err
	}
	defer release()
	return m.getOrSpawnForKeyChatLocked(key)
}
func (m *ChordManager) getOrSpawnForKeyChatLocked(key string) (*ChordProcess, error) {
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	if m.shuttingDown.Load() {
		return nil, ErrManagerShuttingDown
	}
	releaseKeyLock := m.acquireKeyLock(key)
	defer releaseKeyLock()

	if m.transferBlocked(key) {
		return nil, ErrSessionTransferred
	}
	if p := m.GetProcessForKey(key); p != nil {
		if p.transferActive() {
			return nil, ErrSessionTransferred
		}
		if p.Alive() || p.cmd == nil && p.stdin != nil {
			return p, nil
		}
		p.mu.RLock()
		guarded := p.hasGuardedInputsLocked()
		p.mu.RUnlock()
		if guarded {
			return nil, errUnresolvedInputs
		}
		m.removeProcessIfCurrent(key, p)
	}
	workspaceID, _, _ := parseProcessKey(key)
	if workspaceID == "" {
		return nil, fmt.Errorf("invalid process key %q", key)
	}
	cfg := m.Config()
	if cfg == nil || cfg.WorkspaceByID(workspaceID) == nil {
		return nil, nil
	}
	return m.spawnWithArgsForKeyLocked(key, m.spawnArgsForKey(key)...)
}

func (m *ChordManager) spawnArgsForKey(key string) []string {
	if m == nil || m.pins == nil {
		return nil
	}
	sid := strings.TrimSpace(m.pins.Get(pinKeyForBinding(key)))
	if sid == "" {
		return nil
	}
	return []string{"--resume", sid}
}

func (m *ChordManager) clearSessionPinForKey(key string) error {
	if m == nil || m.pins == nil {
		return nil
	}
	return m.pins.Set(pinKeyForBinding(key), "")
}

// cancelDeliveryLocked retires output when the manager removes this runtime.
// The delivery fields are guarded by mgr.mu, which the caller must hold.
func (p *ChordProcess) cancelDeliveryLocked() {
	if p.deliveryCancel != nil {
		p.deliveryCancel()
	}
}

func (m *ChordManager) removeProcessIfCurrent(key string, p *ChordProcess) {
	if m == nil || p == nil {
		return
	}
	m.mu.Lock()
	if m.procs[key] == p {
		p.cancelDeliveryLocked()
		delete(m.procs, key)
	}
	m.mu.Unlock()
}

// shutdownSignal returns a channel closed when StopAll begins. Background loops
// select on it to stop promptly during gateway shutdown. A manager built as a
// struct literal (tests) has a nil channel, and selecting on a nil channel
// blocks forever, which leaves those managers unaffected.
func (m *ChordManager) shutdownSignal() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.shutdownCh
}

// requestShutdown closes the shutdown signal exactly once. StopAll may be called
// repeatedly and struct-literal managers may have no channel, so the close must
// stay idempotent and nil-safe.
func (m *ChordManager) requestShutdown() {
	if m == nil {
		return
	}
	m.shutdownOnce.Do(func() {
		if m.shutdownCh != nil {
			close(m.shutdownCh)
		}
	})
}

// StopAll terminates all managed processes (best-effort) and clears the process map.
func (m *ChordManager) StopAll(grace time.Duration) {
	m.shuttingDown.Store(true)
	m.requestShutdown()
	m.lifecycleGate.Lock()
	defer m.lifecycleGate.Unlock()

	m.mu.Lock()
	procs := make([]*ChordProcess, 0, len(m.procs))
	for _, p := range m.procs {
		p.cancelDeliveryLocked()
		procs = append(procs, p)
	}
	m.procs = make(map[string]*ChordProcess)
	m.mu.Unlock()

	for _, p := range procs {
		p.TerminateGroup(grace)
	}
}

func (m *ChordManager) StopProcessKey(key string) {
	release, err := m.acquireChatOperation(key)
	if err != nil {
		return
	}
	defer release()
	m.stopProcessKeyChatLocked(key)
}
func (m *ChordManager) stopProcessKeyChatLocked(key string) {
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	releaseKeyLock := m.acquireKeyLock(key)
	defer releaseKeyLock()

	m.mu.Lock()
	p, ok := m.procs[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	p.cancelDeliveryLocked()
	delete(m.procs, key)
	m.mu.Unlock()
	p.TerminateGroup(2 * time.Second)
}

func (m *ChordManager) SpawnWithArgsForKey(key string, extraArgs ...string) (*ChordProcess, error) {
	release, err := m.acquireChatOperation(key)
	if err != nil {
		return nil, err
	}
	defer release()
	return m.spawnWithArgsForKeyChatLocked(key, extraArgs...)
}
func (m *ChordManager) spawnWithArgsForKeyChatLocked(key string, extraArgs ...string) (*ChordProcess, error) {
	m.lifecycleGate.RLock()
	defer m.lifecycleGate.RUnlock()
	if m.shuttingDown.Load() {
		return nil, ErrManagerShuttingDown
	}
	releaseKeyLock := m.acquireKeyLock(key)
	defer releaseKeyLock()
	if m.transferBlocked(key) {
		return nil, ErrSessionTransferred
	}
	return m.spawnWithArgsForKeyLocked(key, extraArgs...)
}

// spawnWithArgsForKeyLocked replaces and starts one process. The caller holds
// lifecycleGate for reading and the lifecycle lock for key.
func (m *ChordManager) spawnWithArgsForKeyLocked(key string, extraArgs ...string) (*ChordProcess, error) {
	m.mu.Lock()
	// Stop existing process for this key.
	if p, ok := m.procs[key]; ok {
		p.cancelDeliveryLocked()
		delete(m.procs, key)
		m.mu.Unlock()
		p.TerminateGroup(2 * time.Second)
	} else {
		m.mu.Unlock()
	}

	workspaceID, _, _ := parseProcessKey(key)
	if workspaceID == "" {
		return nil, fmt.Errorf("invalid process key %q", key)
	}

	cfg := m.Config()
	if cfg == nil {
		return nil, fmt.Errorf("workspace %s not found", workspaceID)
	}
	ws := cfg.WorkspaceByID(workspaceID)
	if ws == nil {
		return nil, fmt.Errorf("workspace %s not found", workspaceID)
	}

	m.mu.RLock()
	onEvent := m.onEvent
	m.mu.RUnlock()
	p, err := m.spawn(ws, key, onEvent, extraArgs...)
	if err != nil {
		return nil, err
	}

	return p, nil
}

// spawn starts a new chord headless process for the given workspace.
func (m *ChordManager) spawn(ws *config.Workspace, key string, onEvent func(key string, eventType string, state ControlState), extraArgs ...string) (*ChordProcess, error) {
	transferID, instance := transferRestoreArgs(extraArgs)
	if instance == "" {
		instance = "binding-" + rand.Text()
	}
	workDir, err := resolveTakeoverWorkDir(ws.Path)
	if err != nil {
		return nil, err
	}
	args := []string{"headless", "-d", workDir}
	managed := m.gatewayManagedSupported()
	if transferID != "" && !managed {
		return nil, errors.New("unsupported takeover binary")
	}
	if managed {
		args = append(args, "--gateway-managed")
	}
	if _, specified := transferRestoreArgs(extraArgs); managed && specified == "" {
		args = append(args, "--gateway-instance", instance)
	}
	if dir := m.Config().ChordStateDir; dir != "" {
		args = append(args, "--state-dir", config.Expand(dir))
	}
	args = append(args, extraArgs...)

	cfg := m.Config()
	chordBinary := cfg.ChordBinary()
	chordBinaryMeta := buildinfo.MetadataForPath(chordBinary)
	cmd := exec.Command(chordBinary, args...)
	// Bound Wait if a descendant keeps the captured stderr pipe open after
	// the headless leader exits. Stdout is drained by readLoop.
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = loginShellEnv()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()
		return nil, err
	}

	// Put the child in its own process group when supported so we can terminate the whole tree.
	configureProcessGroup(cmd)

	stderr := newTailBuffer(64 * 1024)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		stdoutPipe.Close()
		return nil, err
	}

	pgid := processGroupID(cmd)

	ctx, cancel := context.WithCancel(context.Background())

	p := &ChordProcess{
		waitDone:            make(chan struct{}),
		bindingInstanceID:   instance,
		workspacePath:       "",
		restoringTransferID: transferID,
		readySignal:         make(chan struct{}),
		cmd:                 cmd,
		stdin:               stdinPipe,
		cancel:              cancel,
		key:                 key,
		workspaceID:         ws.ID,
		lastActivity:        time.Now(),
		autoRestart:         true,
		mgr:                 m,
		onEvent:             onEvent,
		stderr:              stderr,
		pgid:                pgid,
	}
	if m.admission != nil && imTypeForKey(key) != "console" {
		imCfg := m.Config().IMConfigByType(imTypeForKey(key))
		owner := imCfg.OwnerIdentity()
		_, _, chat := parseProcessKey(key)
		if owner != nil {
			id := im.ChatIdentity{Platform: imTypeForKey(key), AccountID: owner.AccountID, ChatID: chat}
			m.admission.mu.Lock()
			p.gate = m.admission.gates[id]
			m.admission.mu.Unlock()
			if p.gate != nil {
				p.lease, _, _ = p.gate.Snapshot()
			}
		}
		original := p.onEvent
		p.onEvent = func(k, e string, state ControlState) {
			if p.lease == nil || p.lease.Context().Err() != nil {
				return
			}
			if original != nil {
				state.BindingInstanceID = p.bindingInstanceID
				original(k, e, state)
			}
		}
	}
	if m.admission != nil && imTypeForKey(key) == "console" {
		_, platform, chat := parseProcessKey(key)
		m.admission.mu.Lock()
		p.gate = m.admission.gates[im.ChatIdentity{Platform: platform, ChatID: chat}]
		m.admission.mu.Unlock()
		if p.gate != nil {
			p.lease, _, _ = p.gate.Snapshot()
		}
	}
	p.bindingGuard = m.transferBlocked(key)
	if p.bindingGuard {
		p.autoRestart = false
	}
	p.state.BindingInstanceID = instance
	for i := 0; i+1 < len(extraArgs); i++ {
		if extraArgs[i] == "--resume" {
			p.expectedSessionID = extraArgs[i+1]
		}
	}
	p.state.UpdatedAt = time.Now().Format(time.RFC3339)

	log.Infof("[%v] chord process spawned pid=%v dir=%v chord_binary=%v chord_binary_mtime=%v", processLogContext(key, p.state),
		cmd.Process.Pid,
		ws.Path,
		chordBinaryMeta.Path,
		chordBinaryMeta.MTime,
	)

	// Restored questions can arrive immediately; their presentation commands
	// must be able to find this process before stdout callbacks start.
	m.mu.Lock()
	m.procs[key] = p
	m.mu.Unlock()
	go p.readLoop(ctx, stdoutPipe)

	// Send subscribe to limit events to control-plane essentials.
	// Core delivery guarantees stay enabled for assistant messages,
	// confirmation flows, idle state, and SubAgent completion summaries.
	// Optional visibility is configured in gateway.
	// Subscription errors are surfaced through the headless error event.
	events := configuredHeadlessSubscribeEvents(cfg)
	if !managed {
		events = slices.DeleteFunc(events, func(event string) bool { return strings.HasPrefix(event, "session_transfer_") })
	}
	if err := p.SendCommand(map[string]any{
		"type":   "subscribe",
		"events": events,
	}); err != nil {
		log.Warnf("failed to send subscribe command workspace=%v key=%v error=%v", ws.ID, key, err)
	}

	return p, nil
}

// BindingInstanceID reads only the identity needed to reject stale output.
func (p *ChordProcess) BindingInstanceID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state.BindingInstanceID
}

// State returns a copy of the current control state.
func (p *ChordProcess) State() ControlState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	state := p.state
	state.MCP = p.state.MCP.clone()

	state.InputQueue = state.InputQueue.clone()
	return state
}

// envelopeWaiters holds channels waiting for one typed response envelope.
// add/remove/notify must be called with the owning ChordProcess mutex held;
// waitForEnvelope takes the lock itself, so callers pass the waiter field
// without holding it.
type envelopeWaiters[T any] struct {
	waiters []chan T
}

func (w *envelopeWaiters[T]) add(ch chan T) {
	w.waiters = append(w.waiters, ch)
}

func (w *envelopeWaiters[T]) remove(target chan T) {
	for i, ch := range w.waiters {
		if ch == target {
			w.waiters = append(w.waiters[:i], w.waiters[i+1:]...)
			return
		}
	}
}

// notify delivers v to all pending waiters and clears the list.
func (w *envelopeWaiters[T]) notify(v T) {
	if len(w.waiters) == 0 {
		return
	}
	for _, ch := range w.waiters {
		select {
		case ch <- v:
		default:
		}
	}
	w.waiters = nil
}

// waitForEnvelope registers a typed waiter, sends cmd, and waits for the
// matching response envelope. Returns ctx.Err() if the context expires first.
func waitForEnvelope[T any](ctx context.Context, p *ChordProcess, waiters *envelopeWaiters[T], cmd map[string]any) (T, error) {
	var zero T
	ch := make(chan T, 1)
	p.mu.Lock()
	waiters.add(ch)
	p.mu.Unlock()

	if err := p.SendCommand(cmd); err != nil {
		p.mu.Lock()
		waiters.remove(ch)
		p.mu.Unlock()
		return zero, err
	}

	select {
	case v := <-ch:
		return v, nil
	case <-ctx.Done():
		p.mu.Lock()
		waiters.remove(ch)
		p.mu.Unlock()
		return zero, ctx.Err()
	}
}

// WaitStatus sends a status command and waits for the next status_response.
// Returns the resulting ControlState, or ctx.Err() if the context expires first.
func (p *ChordProcess) WaitStatus(ctx context.Context, questionIDs ...string) (ControlState, error) {
	p.mu.Lock()
	var ids []string
	for id, question := range p.state.Questions {
		if question != nil && question.Outcome == "" && !slices.Contains(questionIDs, id) {
			ids = append(ids, id)
		}
	}
	p.mu.Unlock()
	slices.Sort(ids)
	ids = append(slices.Clone(questionIDs), ids...)
	if len(ids) > 32 {
		ids = ids[:32]
	}
	state, err := waitForEnvelope(ctx, p, &p.statusWaiters, map[string]any{"type": "status", "question_ids": ids})
	state.InputQueue = state.InputQueue.clone()
	return state, err
}

// WaitRoleList sends a role list command and waits for the role_response.
// Returns the ordered list of main-mode roles and the current role.
func (p *ChordProcess) WaitRoleList(ctx context.Context) (RoleResponse, error) {
	p.roleMu.Lock()
	defer p.roleMu.Unlock()
	return waitForEnvelope(ctx, p, &p.roleWaiters, map[string]any{"type": "role", "action": "list"})
}

// WaitRoleSwitch requests switching to the named role and waits for the
// role_response, which reports ok=false with a message when the switch is
// rejected (unknown/not-available role, already active, pending handoff).
func (p *ChordProcess) WaitRoleSwitch(ctx context.Context, role string) (RoleResponse, error) {
	p.roleMu.Lock()
	defer p.roleMu.Unlock()
	return waitForEnvelope(ctx, p, &p.roleWaiters, map[string]any{"type": "role", "action": "set", "role": role})
}

func configuredHeadlessSubscribeEvents(cfg *config.Config) []string {
	// Default subscribed events (per docs/event-visibility.md):
	// activity, assistant_message, confirm_request, confirm_resolved, question_request, question_updated, question_resolved,
	// handoff_request, handoff_cancelled, idle, error, notification,
	// done_completion, local_shell_result, agent_done, role_change,
	// compaction_status, session_switched, background_result, and image_result.
	// background_result is subscribed by default but can be opted out, because
	// finished job results are pushed as chat messages.
	events := []string{"input_queue_changed", "session_transfer_preparing", "session_transfer_status", "session_transfer_saved", "session_transfer_released", "activity", "assistant_message", "confirm_request", "confirm_resolved", "question_request", "question_updated", "question_resolved", "handoff_request", "handoff_cancelled", "idle", "error", "notification", "done_completion", "local_shell_result", "agent_done", "role_change", "compaction_status", "session_switched", "background_result", imageResultEvent, mcpStatusChangedEvent}
	if cfg == nil {
		return events
	}
	if !cfg.EventVisibility.BackgroundResultEnabled() {
		events = slices.DeleteFunc(events, func(event string) bool { return event == "background_result" })
	}
	for _, optional := range []struct {
		enabled bool
		event   string
	}{
		{cfg.EventVisibility.AgentStarted, "agent_started"},
		{cfg.EventVisibility.AgentNotify, "agent_notify"},
		{cfg.EventVisibility.ContextNotice, "context_notice"},
		{cfg.EventVisibility.Info, "info"},
		{cfg.EventVisibility.Toast, "toast"},
		{cfg.EventVisibility.Todos, "todos"},
	} {
		if optional.enabled {
			events = append(events, optional.event)
		}
	}
	return events
}

// Alive returns true if the chord process is still running.
func (p *ChordProcess) Alive() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	return !p.exited
}

func (p *ChordProcess) waitAndRecordExit(cmd *exec.Cmd) int {
	if cmd == nil {
		p.mu.Lock()
		p.exited = true
		exitCode := p.exitCode
		p.mu.Unlock()
		return exitCode
	}
	p.waitOnce.Do(func() {
		if p.waitDone != nil {
			defer close(p.waitDone)
		}
		_ = cmd.Wait()
		exitCode := 0
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		p.mu.Lock()
		p.exited = true
		p.exitCode = exitCode
		p.mu.Unlock()
	})
	p.mu.RLock()
	exitCode := p.exitCode
	p.mu.RUnlock()
	return exitCode
}

// SendCommand writes a JSON command to the chord process stdin.
// The command format matches chord headless stdin protocol:
// {"type":"send","content":"..."}, {"type":"confirm","request_id":"...","action":"allow"}, etc.
func (p *ChordProcess) SendCommand(cmd map[string]any) error {
	if cmd["type"] == "send" && cmd["content"] == "/new" {
		if err := p.refreshPendingInputStatus(context.Background()); err != nil {
			return err
		}
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if cmd["type"] == "send" && cmd["content"] == "/new" && p.hasGuardedInputsLocked() {
		return errUnresolvedInputs
	}

	if !p.commandAllowedLocked(cmd) {
		return ErrSessionTransferred
	}
	if err := p.writeCommandLocked(data); err != nil {
		return err
	}
	// Update command state before releasing the lock: stdout may already
	// contain its terminal event. Only observed agent work opens a turn window;
	// cancellation preserves the current busy state, including accepted inputs
	// that Core has not yet started. Shell work is tracked independently.
	switch cmd["type"] {
	case "confirm", "handoff", "cancel", "local_shell":
		now := time.Now()
		switch cmd["type"] {
		case "confirm", "handoff":
			p.state.setAgentBusy(true)
		case "local_shell":
			p.state.beginLocalShell(now)
		}
		p.state.LastPushAt = now
		p.state.InternalEventsSinceLastPush = 0
	}
	return nil
}

// writeCommandLocked writes one complete JSONL command under p.mu, shared by
// control commands and inputs that also register a consumption receipt.
func (p *ChordProcess) writeCommandLocked(data []byte) error {
	if p.lease != nil && p.lease.Context().Err() != nil {
		return context.Canceled
	}
	if p.stdin == nil {
		return io.ErrClosedPipe
	}
	data = append(data, '\n')
	n, err := p.stdin.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("write headless command: %w", err)
	}
	return nil
}

// MarkVisibleOutput records that the user just received an IM-visible message
// from the gateway: resets the long-running reminder counters and stamps
// LastPushAt so reminders fire only after a fresh quiet period.
func (p *ChordProcess) MarkVisibleOutput(now time.Time) {
	p.mu.Lock()
	p.state.LastPushAt = now
	p.state.InternalEventsSinceLastPush = 0
	p.mu.Unlock()
}

func imTypeForKey(key string) string { _, platform, _ := parseProcessKey(key); return platform }
