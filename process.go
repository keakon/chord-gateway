package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
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

	// Phase tracking for long-running detection
	phaseStartedAt time.Time
	lastPhaseAlert string

	// Auto-restart on crash
	autoRestart      bool
	stoppedByGateway bool
	mgr              *ChordManager // back-reference for auto-restart

	// Callback for events that need IM notification.
	onEvent func(key string, eventType string, state ControlState)
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
	cfg     *config.Config
	procs   map[string]*ChordProcess // processKey.String() → active process
	onEvent func(key string, eventType string, state ControlState)

	pins *sessionPinStore
}

// TerminateGroup attempts to gracefully stop the chord process and, if needed,
// force-kill its whole process group.
func (p *ChordProcess) TerminateGroup(grace time.Duration) {
	if p == nil {
		return
	}
	p.exitOnce.Do(func() {
		// Mark as stopped by gateway to avoid crash restarts.
		p.mu.Lock()
		p.stoppedByGateway = true
		// Close stdin first to let chord exit gracefully.
		if p.stdin != nil {
			_ = p.stdin.Close()
			p.stdin = nil
		}
		cmd := p.cmd
		pgid := p.pgid
		p.mu.Unlock()

		// If no process, nothing to do.
		if cmd == nil || cmd.Process == nil {
			return
		}

		// Best-effort: send SIGTERM to process group.
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		} else {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}

		// Wait for graceful exit up to grace.
		if grace <= 0 {
			grace = 2 * time.Second
		}
		done := make(chan struct{})
		go func() {
			p.waitOnce.Do(func() {
				_ = cmd.Wait()
			})
			close(done)
		}()

		select {
		case <-done:
			return
		case <-time.After(grace):
		}

		// Force kill.
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = cmd.Process.Kill()
		}
	})
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

	return &ChordManager{
		cfg:   cfg,
		procs: make(map[string]*ChordProcess),
		pins:  pins,
	}
}

// SetOnEvent registers a callback invoked when a chord process emits a notable event.
func (m *ChordManager) SetOnEvent(fn func(key, eventType string, state ControlState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onEvent = fn
}

// GetProcess returns the active process for a workspace, or nil if none exists.
func (m *ChordManager) GetProcess(workspaceID string) *ChordProcess {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Backward compat: return the first matching workspace process (if any).
	for k, p := range m.procs {
		w, _, _ := parseProcessKey(k)
		if w == workspaceID {
			return p
		}
	}
	return nil
}

// GetOrSpawn returns the active process for a workspace, or spawns one if none exists.
func (m *ChordManager) GetOrSpawn(workspaceID string) (*ChordProcess, error) {
	// Backward compat for legacy call sites: spawn a single shared chat key.
	// New code should call GetOrSpawnForKey.
	return m.GetOrSpawnForKey(processKey{workspaceID: workspaceID}.String())
}

func (m *ChordManager) GetOrSpawnForKey(key string) (*ChordProcess, error) {
	m.mu.Lock()
	if p, ok := m.procs[key]; ok {
		m.mu.Unlock()
		return p, nil
	}

	workspaceID, _, _ := parseProcessKey(key)
	if workspaceID == "" {
		// Legacy key: treat as workspace-only.
		workspaceID = key
	}

	// Find workspace config.
	var ws *config.Workspace
	for i := range m.cfg.Workspaces {
		if m.cfg.Workspaces[i].ID == workspaceID {
			ws = &m.cfg.Workspaces[i]
			break
		}
	}
	if ws == nil {
		m.mu.Unlock()
		return nil, nil
	}

	onEvent := m.onEvent
	m.mu.Unlock()

	args := m.spawnArgsForKey(key)
	p, err := m.spawn(ws, key, onEvent, args...)
	if err != nil {
		return nil, err
	}
	p.workspaceID = workspaceID

	m.mu.Lock()
	m.procs[key] = p
	m.mu.Unlock()

	return p, nil
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

// StopProcess closes stdin for the workspace's chord process and waits for it to exit.
func (m *ChordManager) StopProcess(workspaceID string) {
	// Backward compat: stop all processes for this workspace.
	m.mu.Lock()
	keys := make([]string, 0)
	for k := range m.procs {
		w, _, _ := parseProcessKey(k)
		if w == workspaceID {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()
	for _, k := range keys {
		m.StopProcessKey(k)
	}
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
		workspaceID = key
	}

	var ws *config.Workspace
	for i := range m.cfg.Workspaces {
		if m.cfg.Workspaces[i].ID == workspaceID {
			ws = &m.cfg.Workspaces[i]
			break
		}
	}
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

// SpawnWithArgs creates a new chord process with extra arguments (e.g. --resume <id>).
// If no extra args are provided, a fresh session is created (no --continue).
func (m *ChordManager) SpawnWithArgs(workspaceID string, extraArgs ...string) (*ChordProcess, error) {
	// Backward compat: spawn a single shared chat key.
	// New code should call SpawnWithArgsForKey.
	key := processKey{workspaceID: workspaceID}.String()
	return m.SpawnWithArgsForKey(key, extraArgs...)
}

// spawn starts a new chord headless process for the given workspace.
func (m *ChordManager) spawn(ws *config.Workspace, key string, onEvent func(key string, eventType string, state ControlState), extraArgs ...string) (*ChordProcess, error) {
	args := []string{"headless", "-d", ws.Path}
	args = append(args, extraArgs...)

	cmd := exec.Command(m.cfg.ChordBinary(), args...)
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

	// Put the child in its own process group so we can terminate the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stderr := newTailBuffer(64 * 1024)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		stdoutPipe.Close()
		return nil, err
	}

	pgid, _ := syscall.Getpgid(cmd.Process.Pid)

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

	slog.Info("chord process spawned",
		"workspace", ws.ID,
		"pid", cmd.Process.Pid,
		"dir", ws.Path,
	)

	go p.readLoop(ctx, stdoutPipe)

	// Send subscribe to limit events to control-plane essentials.
	// Core delivery guarantees stay enabled for assistant_message /
	// confirm_request / question_request / idle. Optional visibility is configured
	// in gateway.
	// If chord headless is too old, it may emit an error; ignore.
	_ = p.SendCommand(map[string]any{
		"type":   "subscribe",
		"events": configuredHeadlessSubscribeEvents(m.cfg),
	})

	return p, nil
}

// State returns a copy of the current control state.
func (p *ChordProcess) State() ControlState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func configuredHeadlessSubscribeEvents(cfg *config.Config) []string {
	// Default events always subscribed (per docs/event-visibility.md):
	// assistant_message, confirm_request, question_request, idle, error, notification.
	events := []string{"assistant_message", "confirm_request", "question_request", "idle", "error", "notification"}
	if cfg == nil {
		return events
	}
	// Optional visibility events (configured via event_visibility).
	if cfg.EventVisibility.Activity {
		events = append(events, "activity")
	}
	if cfg.EventVisibility.AgentDone {
		events = append(events, "agent_done")
	}
	if cfg.EventVisibility.Info {
		events = append(events, "info")
	}
	if cfg.EventVisibility.Toast {
		events = append(events, "toast")
	}
	if cfg.EventVisibility.ToolResult {
		events = append(events, "tool_result")
	}
	if cfg.EventVisibility.Todos {
		events = append(events, "todos")
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

// readLoop reads stdout from the chord process, parses JSON envelopes,
// updates state, and calls onEvent for notable events.
func (p *ChordProcess) readLoop(ctx context.Context, stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var env HeadlessEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			slog.Warn("failed to parse headless envelope", "line", string(line), "error", err)
			continue
		}

		p.processEnvelope(&env)
	}

	// stdout EOF — chord process has exited.
	p.handleExit()
}

// processEnvelope updates ControlState based on the envelope type and calls onEvent.
func (p *ChordProcess) processEnvelope(env *HeadlessEnvelope) {
	p.mu.Lock()

	p.lastActivity = time.Now()
	p.state.UpdatedAt = time.Now().Format(time.RFC3339)

	var eventType string

	switch env.Type {
	case "ready":
		var payload struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			if strings.TrimSpace(payload.SessionID) != "" {
				p.state.SessionID = payload.SessionID
				if p.mgr != nil && p.mgr.pins != nil {
					_ = p.mgr.pins.Set(p.key, payload.SessionID)
				}
			}
		}
		slog.Info("gateway event", "event", "ready", "raw_type", "ready", "key", p.key, "workspace", p.workspaceID, "channel", func() string { _, imType, _ := parseProcessKey(p.key); return imType }(), "session_id", p.state.SessionID)
		// No notification.
		eventType = ""

	case "activity":
		p.state.Busy = true
		p.lastActivity = time.Now()
		if p.state.LastPushAt.IsZero() {
			p.state.LastPushAt = time.Now()
		}
		var payload struct {
			AgentID string `json:"agent_id"`
			Type    string `json:"type"`
			Detail  string `json:"detail"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			// Track phase changes for long-running detection
			if payload.Type != p.state.Phase {
				p.phaseStartedAt = time.Now()
				p.lastPhaseAlert = "" // reset so a new alert can fire
			}
			p.state.Phase = payload.Type
			p.state.PhaseDetail = payload.Detail
		}
		eventType = "activity"

	case "idle":
		p.state.Busy = false
		var payload struct {
			LastOutcome string `json:"last_outcome"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastOutcome = payload.LastOutcome
		}
		p.state.PendingConfirm = nil
		p.state.PendingQuestion = nil
		p.state.LastError = ""
		// AgentDoneSummary is preserved for the idle notification
		// (cleared when the next activity starts).
		eventType = "idle"

	case "confirm_request":
		var payload ConfirmPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.PendingConfirm = &payload
		}
		eventType = "confirm_request"

	case "question_request":
		var payload QuestionPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.PendingQuestion = &payload
		}
		eventType = "question_request"

	case "error":
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastError = payload.Message
		}
		eventType = "error"
	case "notification":
		var payload NotificationPayload
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastNotification = &payload
		}
		eventType = "notification"

	case "agent_done":
		p.lastActivity = time.Now()
		var payload struct {
			AgentID string `json:"agent_id"`
			TaskID  string `json:"task_id"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.AgentDoneSummary = payload.Summary
		}
		eventType = "agent_done"

	case "info":
		p.lastActivity = time.Now()
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.InfoMessage = payload.Message
		}
		eventType = "info"

	case "toast":
		p.lastActivity = time.Now()
		var payload struct {
			Message string `json:"message"`
			Level   string `json:"level"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.ToastMessage = payload.Message
			p.state.ToastLevel = payload.Level
		}
		eventType = "toast"

	case "status_response":
		var resp StatusResponse
		if err := json.Unmarshal(env.Payload, &resp); err == nil {
			p.state.SessionID = resp.SessionID
			p.state.Busy = resp.Busy
			p.state.Phase = resp.Phase
			p.state.PhaseDetail = resp.PhaseDetail
			p.state.PendingConfirm = resp.PendingConfirm
			p.state.PendingQuestion = resp.PendingQuestion
			p.state.LastError = resp.LastError
			p.state.UpdatedAt = resp.UpdatedAt
			p.state.LastOutcome = resp.LastOutcome
			p.state.LastStatusResponseAt = time.Now()
		}
		// No onEvent — solicited response.

	case "subscribe_response":
		// No onEvent — ack response.

	case "tool_result":
		var payload struct {
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Status  string `json:"status"`
			AgentID string `json:"agent_id"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			p.state.LastToolResult = &ToolResultInfo{
				CallID:  payload.CallID,
				Name:    payload.Name,
				Status:  payload.Status,
				AgentID: payload.AgentID,
			}
			p.state.ToolCallsSinceLastPush++
			p.state.UpdatedAt = time.Now().Format(time.RFC3339)
			p.lastActivity = time.Now()
		}
		eventType = "tool_result"

	case "assistant_message":
		var payload struct {
			Text      string `json:"text"`
			AgentID   string `json:"agent_id"`
			ToolCalls int    `json:"tool_calls"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err == nil {
			if strings.TrimSpace(payload.Text) != "" {
				p.state.LastAssistantText = payload.Text
				eventType = "assistant_message"
			} else {
				slog.Debug("gateway assistant_message had empty text; skipping notification",
					"key", p.key,
					"workspace", p.workspaceID,
					"agent_id", payload.AgentID,
					"tool_calls", payload.ToolCalls,
				)
			}
			p.state.LastAssistantToolCalls = payload.ToolCalls
			p.state.ToolCallsSinceLastPush = 0
			p.state.LastPushAt = time.Now()
		}

	case "todos":
		// Support both formats:
		// a) New: {"todos": [...]}  (chord headless current format)
		// b) Old: [...]  (bare array)
		var items []TodoItem
		// Try new format first.
		var wrapper struct {
			Todos []TodoItem `json:"todos"`
		}
		if json.Unmarshal(env.Payload, &wrapper) == nil && wrapper.Todos != nil {
			items = wrapper.Todos
			p.lastActivity = time.Now()
		} else if err := json.Unmarshal(env.Payload, &items); err != nil {
			slog.Warn("failed to parse todos payload", "key", p.key, "error", err)
		}
		p.state.Todos = items
		if !p.state.LastPushAt.IsZero() {
			p.state.ToolCallsSinceLastPush++
		}
		eventType = "todos"

	case "assistant_rollback":
		p.state.StreamText = ""
		p.state.LastAssistantText = ""
		p.state.LastThinkingText = ""
		eventType = "assistant_rollback"

	default:
		slog.Debug("unknown headless event type", "type", env.Type)
	}

	if eventType != "" {
		_, imType, chatID := parseProcessKey(p.key)
		slog.Info("gateway event",
			"event", eventType,
			"raw_type", env.Type,
			"key", p.key,
			"workspace", p.workspaceID,
			"im", imType,
			"channel", imType,
			"chat_id", chatID,
			"session_id", p.state.SessionID,
			"busy", p.state.Busy,
			"phase", p.state.Phase,
			"last_outcome", p.state.LastOutcome,
			"assistant_text_len", len(p.state.LastAssistantText),
			"assistant_tool_calls", p.state.LastAssistantToolCalls,
			"pending_confirm", p.state.PendingConfirm != nil,
			"pending_question", p.state.PendingQuestion != nil,
			"last_error", p.state.LastError,
		)
	}

	// Capture callback params under lock, then invoke outside lock to prevent
	// deadlock: onEvent → router → proc.Alive/SendCommand → p.mu.
	var (
		onEvent = p.onEvent
		key     = p.key
		state   = p.state // copy
	)
	p.mu.Unlock()

	if eventType != "" && onEvent != nil {
		onEvent(key, eventType, state)
	}
}

// handleExit cleans up when the chord process exits.
func (p *ChordProcess) handleExit() {
	p.mu.Lock()
	p.state.Busy = false
	p.state.UpdatedAt = time.Now().Format(time.RFC3339)
	if p.stdin != nil {
		p.stdin.Close()
		p.stdin = nil
	}
	if p.cancel != nil {
		p.cancel()
	}
	cmd := p.cmd
	pid := 0
	if cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	crashed := !p.stoppedByGateway
	autoRestart := p.autoRestart && crashed
	key := p.key
	state := p.state
	stderr := ""
	if p.stderr != nil {
		stderr = p.stderr.String()
	}
	// If this is an expected init failure (e.g. session lock), don't spam auto-restart.
	// Cobra prints errors to stderr; chord exits with code 1 on init errors.
	if strings.Contains(stderr, "acquire session lock") {
		autoRestart = false
	}
	p.mu.Unlock()

	// Serialize Cmd.Wait calls. TerminateGroup may call Wait concurrently.
	// Waiting here guarantees ProcessState is fully populated before reading it.
	if cmd != nil {
		p.waitOnce.Do(func() {
			_ = cmd.Wait()
		})
	}

	exitCode := 0
	if cmd != nil && cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	slog.Info("chord process exited", "key", key, "pid", pid, "exit_code", exitCode, "crashed", crashed)
	if crashed && strings.TrimSpace(stderr) != "" {
		// Keep stderr short; full details are usually in chord.log.
		slog.Warn("chord process stderr", "key", key, "pid", pid, "stderr", truncateStderr(stderr, 2000))
	}

	if p.onEvent != nil {
		p.onEvent(key, "exit", state)
	}

	if autoRestart {
		go func() {
			slog.Info("auto-restarting crashed chord process in 5s", "key", key)
			time.Sleep(5 * time.Second)
			// Use the manager to respawn; it handles the procs map.
			if p.mgr != nil {
				if _, err := p.mgr.GetOrSpawnForKey(key); err != nil {
					slog.Error("auto-restart failed", "key", key, "error", err)
				} else {
					slog.Info("auto-restart succeeded", "key", key)
				}
			}
		}()
	}
}

// IdleCheckLoop periodically checks all processes and closes idle ones.
func (m *ChordManager) IdleCheckLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		m.mu.Lock()
		idle := make([]*ChordProcess, 0)
		timeout := m.cfg.IdleTimeoutDuration()
		type longRunningAlert struct {
			workspaceID string
			state       ControlState
		}
		var alerts []longRunningAlert
		for wid, p := range m.procs {
			p.mu.Lock()
			if time.Since(p.lastActivity) > timeout {
				idle = append(idle, p)
			}
			// Long-running phase alert
			if p.state.Busy && !p.phaseStartedAt.IsZero() && time.Since(p.phaseStartedAt) > 5*time.Minute {
				if p.lastPhaseAlert != p.state.Phase {
					p.lastPhaseAlert = p.state.Phase
					alerts = append(alerts, longRunningAlert{workspaceID: wid, state: p.state})
				}
			}
			p.mu.Unlock()
		}
		// Remove idle procs from map.
		for _, p := range idle {
			delete(m.procs, p.key)
		}
		onEvent := m.onEvent
		m.mu.Unlock()

		// Fire long-running alerts outside all locks.
		if onEvent != nil {
			for _, a := range alerts {
				onEvent(a.workspaceID, "long_running", a.state)
			}
		}

		// Close stdin for idle processes to let them exit gracefully.
		// Then, if they don't exit quickly, terminate the whole process group.
		for _, p := range idle {
			slog.Info("idle timeout, stopping chord process", "workspace", p.workspaceID)
			p.TerminateGroup(2 * time.Second)
		}
	}
}

// loginShellEnv returns os.Environ() with PATH replaced by the user's login
// shell PATH.  This ensures that binaries installed in user-local paths
// (e.g. ~/.local/bin, /opt/homebrew/bin) are findable even when the gateway
// is launched from a context that doesn't source .zshrc / .bash_profile.
var loginShellEnv = sync.OnceValue(func() []string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	// Run a login shell that just prints PATH; -l (--login) sources
	// /etc/profile + ~/.profile (or ~/.zprofile / ~/.zshenv etc.).
	out, err := exec.Command(shell, "-l", "-c", "echo $PATH").Output()
	if err != nil {
		slog.Warn("failed to get login shell PATH, using current env", "error", err)
		return os.Environ()
	}
	loginPATH := strings.TrimSpace(string(out))
	if loginPATH == "" {
		return os.Environ()
	}
	// Merge: keep current env PATH entries that aren't in login PATH,
	// then append login PATH entries — this preserves runtime-added
	// paths (e.g. nvm) while also picking up login-only paths.
	currentPATH := os.Getenv("PATH")
	var merged []string
	if currentPATH != "" {
		seen := make(map[string]bool)
		for _, p := range append(
			strings.Split(currentPATH, ":"),
			strings.Split(loginPATH, ":")...,
		) {
			if p != "" && !seen[p] {
				seen[p] = true
				merged = append(merged, p)
			}
		}
	} else {
		merged = strings.Split(loginPATH, ":")
	}
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PATH=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, "PATH="+strings.Join(merged, ":"))
	return env
})
