package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/keakon/golog/log"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/keakon/chord-gateway/config"
)

// ChordProcess manages a single chord headless child process.
type ChordProcess struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc

	waitOnce sync.Once
	exitOnce sync.Once
	stderr   *tailBuffer

	pgid int

	key          string
	workspaceID  string
	state        ControlState
	lastActivity time.Time

	// Auto-restart on crash
	autoRestart      bool
	stoppedByGateway bool
	mgr              *ChordManager // back-reference for auto-restart

	// Callback for events that need IM notification.
	onEvent func(key string, eventType string, state ControlState)

	// statusWaiters are notified when a status_response envelope arrives.
	statusWaiters []chan ControlState
}

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

func truncateStderr(s string, max int) string {
	if max <= 0 {
		max = 2000
	}
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

// ChordManager manages chord headless processes, one per workspace.
type ChordManager struct {
	mu      sync.Mutex
	cfg     atomic.Pointer[config.Config]
	procs   map[string]*ChordProcess // processKey.String() → active process
	onEvent func(key string, eventType string, state ControlState)

	pins *sessionPinStore
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
	_ = pins.Load()

	m := &ChordManager{
		procs: make(map[string]*ChordProcess),
		pins:  pins,
	}
	m.cfg.Store(cfg)
	return m
}

// Config returns the current config snapshot.
func (m *ChordManager) Config() *config.Config {
	return m.cfg.Load()
}

// setConfig replaces the active config atomically.
func (m *ChordManager) setConfig(cfg *config.Config) {
	m.cfg.Store(cfg)
}

// SetOnEvent registers a callback invoked when a chord process emits a notable event.
func (m *ChordManager) SetOnEvent(fn func(key, eventType string, state ControlState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEvent = fn
}

// GetProcessForKey returns the active process for a process key, or nil if none exists.
func (m *ChordManager) GetProcessForKey(key string) *ChordProcess {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.procs[key]
}

func (m *ChordManager) GetOrSpawnForKey(key string) (*ChordProcess, error) {
	if p := m.GetProcessForKey(key); p != nil {
		return p, nil
	}
	workspaceID, _, _ := parseProcessKey(key)
	if workspaceID == "" {
		return nil, fmt.Errorf("invalid process key %q", key)
	}
	cfg := m.cfg.Load()
	if cfg == nil || cfg.WorkspaceByID(workspaceID) == nil {
		return nil, nil
	}
	return m.SpawnWithArgsForKey(key, m.spawnArgsForKey(key)...)
}

func (m *ChordManager) spawnArgsForKey(key string) []string {
	if m == nil || m.pins == nil {
		return nil
	}
	sid := strings.TrimSpace(m.pins.Get(key))
	if sid == "" {
		return nil
	}
	return []string{"--resume", sid}
}

// StopAll terminates all managed processes (best-effort) and clears the process map.
func (m *ChordManager) StopAll(grace time.Duration) {
	m.mu.Lock()
	procs := make([]*ChordProcess, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	m.procs = make(map[string]*ChordProcess)
	m.mu.Unlock()

	for _, p := range procs {
		p.TerminateGroup(grace)
	}
}

func (m *ChordManager) StopProcessKey(key string) {
	m.mu.Lock()
	p, ok := m.procs[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.procs, key)
	m.mu.Unlock()
	p.TerminateGroup(2 * time.Second)
}

func (m *ChordManager) SpawnWithArgsForKey(key string, extraArgs ...string) (*ChordProcess, error) {
	m.mu.Lock()
	// Stop existing process for this key.
	if p, ok := m.procs[key]; ok {
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

	cfg := m.cfg.Load()
	if cfg == nil {
		return nil, fmt.Errorf("workspace %s not found", workspaceID)
	}
	ws := cfg.WorkspaceByID(workspaceID)
	if ws == nil {
		return nil, fmt.Errorf("workspace %s not found", workspaceID)
	}

	onEvent := m.onEvent
	p, err := m.spawn(ws, key, onEvent, extraArgs...)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.procs[key] = p
	m.mu.Unlock()
	return p, nil
}

// spawn starts a new chord headless process for the given workspace.
func (m *ChordManager) spawn(ws *config.Workspace, key string, onEvent func(key string, eventType string, state ControlState), extraArgs ...string) (*ChordProcess, error) {
	args := []string{"headless", "-d", ws.Path}
	args = append(args, extraArgs...)

	cfg := m.cfg.Load()
	cmd := exec.Command(cfg.ChordBinary(), args...)
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
		cmd:          cmd,
		stdin:        stdinPipe,
		cancel:       cancel,
		key:          key,
		workspaceID:  ws.ID,
		lastActivity: time.Now(),
		autoRestart:  true,
		mgr:          m,
		onEvent:      onEvent,
		stderr:       stderr,
		pgid:         pgid,
	}
	p.state.UpdatedAt = time.Now().Format(time.RFC3339)

	log.Infof("chord process spawned workspace=%v pid=%v dir=%v", ws.ID,
		cmd.Process.Pid,
		ws.Path,
	)

	go p.readLoop(ctx, stdoutPipe)

	// Send subscribe to limit events to control-plane essentials.
	// Core delivery guarantees stay enabled for assistant_message /
	// confirm_request / question_request / idle. Optional visibility is configured
	// in gateway.
	// If chord headless is too old, it may emit an error; ignore.
	if err := p.SendCommand(map[string]any{
		"type":   "subscribe",
		"events": configuredHeadlessSubscribeEvents(cfg),
	}); err != nil {
		log.Warnf("failed to send subscribe command workspace=%v key=%v error=%v", ws.ID, key, err)
	}

	return p, nil
}

// State returns a copy of the current control state.
func (p *ChordProcess) State() ControlState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// WaitStatus sends a status command and waits for the next status_response.
// Returns the resulting ControlState, or ctx.Err() if the context expires first.
func (p *ChordProcess) WaitStatus(ctx context.Context) (ControlState, error) {
	ch := make(chan ControlState, 1)
	p.mu.Lock()
	p.statusWaiters = append(p.statusWaiters, ch)
	p.mu.Unlock()

	if err := p.SendCommand(map[string]any{"type": "status"}); err != nil {
		p.removeStatusWaiter(ch)
		return ControlState{}, err
	}

	select {
	case state := <-ch:
		return state, nil
	case <-ctx.Done():
		p.removeStatusWaiter(ch)
		return ControlState{}, ctx.Err()
	}
}

func (p *ChordProcess) removeStatusWaiter(target chan ControlState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, ch := range p.statusWaiters {
		if ch == target {
			p.statusWaiters = append(p.statusWaiters[:i], p.statusWaiters[i+1:]...)
			return
		}
	}
}

// notifyStatusWaiters delivers state to all pending waiters and clears the list.
// Caller must hold p.mu.
func (p *ChordProcess) notifyStatusWaiters(state ControlState) {
	if len(p.statusWaiters) == 0 {
		return
	}
	for _, ch := range p.statusWaiters {
		select {
		case ch <- state:
		default:
		}
	}
	p.statusWaiters = nil
}

func configuredHeadlessSubscribeEvents(cfg *config.Config) []string {
	// Default events always subscribed (per docs/event-visibility.md):
	// assistant_message, confirm_request, question_request, idle, error, notification.
	events := []string{"assistant_message", "confirm_request", "question_request", "idle", "error", "notification"}
	if cfg == nil {
		return events
	}
	for _, optional := range []struct {
		enabled bool
		event   string
	}{
		{cfg.EventVisibility.Activity, "activity"},
		{cfg.EventVisibility.AgentDone, "agent_done"},
		{cfg.EventVisibility.Info, "info"},
		{cfg.EventVisibility.Toast, "toast"},
		{cfg.EventVisibility.ToolResult, "tool_result"},
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	return p.cmd.ProcessState == nil
}

// SendCommand writes a JSON command to the chord process stdin.
// The command format matches chord headless stdin protocol:
// {"type":"send","content":"..."}, {"type":"confirm","request_id":"...","action":"allow"}, etc.
func (p *ChordProcess) SendCommand(cmd map[string]any) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return io.ErrClosedPipe
	}
	_, err = p.stdin.Write(data)
	return err
}

// SendUserMessage sends a user message command to the chord process.
func (p *ChordProcess) SendUserMessage(content string) error {
	return p.SendCommand(map[string]any{
		"type":    "send",
		"content": content,
	})
}
