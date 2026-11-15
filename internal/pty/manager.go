package pty

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gopty "github.com/aymanbagabas/go-pty"

	"tether/internal/bus"
	"tether/internal/env"
)

// Type aliases for convenience.
type (
	AgentID      = bus.AgentID
	SessionID    = bus.SessionID
	SessionState = bus.SessionState
)

const (
	StateIdle            = bus.StateIdle
	StateRunning         = bus.StateRunning
	StateWaitingForInput = bus.StateWaitingForInput
	StateClosed          = bus.StateClosed

	DefaultIdleTimeout = 3 * time.Second
	DefaultExecTimeout = 30 * time.Second
)

var (
	ErrSessionNotFound = errors.New("terminal session not found for authenticated agent")
	ErrSessionClosed   = errors.New("terminal session is closed")
	ErrSessionBusy     = errors.New("terminal session is already executing a command")
)

// ExecResult contains the sanitized output, exit code, and final session state after execution.
type ExecResult struct {
	SessionID       SessionID    `json:"session_id"`
	Output          string       `json:"output"`
	ExitCode        int          `json:"exit_code"`
	State           SessionState `json:"state"`
	RelCWD          string       `json:"rel_cwd"`
	WaitingForInput bool         `json:"waiting_for_input"`
	TimedOut        bool         `json:"timed_out,omitempty"`
}

// TerminalSession represents an active isolated PTY session belonging to a single AgentID.
type TerminalSession struct {
	mu            sync.RWMutex
	execMu        sync.Mutex
	info          bus.TerminalSession
	ptmx          gopty.Pty
	cmd           *gopty.Cmd
	procGroup     *processGroupHandle
	ring          *RingBuffer
	notifyCh      chan struct{}
	closedCh      chan struct{}
	closeOnce     sync.Once
	activeMarker  string
	pendingNextCD string
}

// Snapshot returns a thread-safe copy of the session's metadata.
func (ts *TerminalSession) Snapshot() bus.TerminalSession {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	cp := ts.info
	if ts.info.ExitCode != nil {
		ec := *ts.info.ExitCode
		cp.ExitCode = &ec
	}
	return cp
}

// Screen returns the last n clean (ANSI-stripped) lines from the session's RingBuffer.
func (ts *TerminalSession) Screen(lines int) []string {
	if lines <= 0 {
		lines = 50
	}
	return ts.ring.LastCleanLines(lines)
}

// RawScreen returns the entire raw/clean text currently in the RingBuffer for TUI rendering.
func (ts *TerminalSession) RawScreen() string {
	return StripANSI(ts.ring.String())
}

// Option configures a Manager.
type Option func(*Manager)

// WithIdleTimeout configures the duration without PTY output before a running command
// is flagged as WAITING_FOR_INPUT.
func WithIdleTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.idleTimeout = d
		}
	}
}

// Manager maintains isolated per-AgentID namespaces of PTY sessions.
type Manager struct {
	mu          sync.RWMutex
	sessions    map[AgentID]map[SessionID]*TerminalSession
	sysInfo     env.SystemInfo
	eventBus    *bus.EventBus
	idleTimeout time.Duration
	seq         atomic.Uint64
	dirtyFlag   atomic.Bool
}

// ConsumeDirty atomically checks and resets the dirty flag used by the 60 FPS TUI render loop.
func (m *Manager) ConsumeDirty() bool {
	return m.dirtyFlag.Swap(false)
}

// MarkDirty marks the PTY manager state as dirty for the next 60 FPS frame.
func (m *Manager) MarkDirty() {
	m.dirtyFlag.Store(true)
}

// NewManager creates a multi-tenant PTY session manager.
func NewManager(sysInfo env.SystemInfo, eventBus *bus.EventBus, opts ...Option) *Manager {
	m := &Manager{
		sessions:    make(map[AgentID]map[SessionID]*TerminalSession),
		sysInfo:     sysInfo,
		eventBus:    eventBus,
		idleTimeout: DefaultIdleTimeout,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// SystemInfo returns the detected host environment information.
func (m *Manager) SystemInfo() env.SystemInfo {
	return m.sysInfo
}

// OpenSession creates and starts a new isolated PTY session inside the agent's workspaceRoot.
func (m *Manager) OpenSession(agentID AgentID, agentName, workspaceRoot, requestedShell, tabName string) (bus.TerminalSession, error) {
	if strings.TrimSpace(string(agentID)) == "" {
		return bus.TerminalSession{}, errors.New("agent_id is required")
	}

	wsAbs, err := env.ResolveSafeSubpath(workspaceRoot, ".")
	if err != nil {
		return bus.TerminalSession{}, fmt.Errorf("invalid workspace root: %w", err)
	}

	shellInfo, err := m.sysInfo.ResolveShell(requestedShell)
	if err != nil {
		return bus.TerminalSession{}, err
	}

	ptmx, err := gopty.New()
	if err != nil {
		return bus.TerminalSession{}, fmt.Errorf("creating pty: %w", err)
	}
	_ = ptmx.Resize(120, 36)

	cmd := ptmx.Command(shellInfo.Path, shellInfo.Args...)
	cmd.Dir = wsAbs
	cmd.Env = buildNonInteractiveEnv(os.Environ())
	cmd.SysProcAttr = configureSysProcAttr()

	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		return bus.TerminalSession{}, fmt.Errorf("starting shell %s in pty: %w", shellInfo.Name, err)
	}

	pg, _ := attachProcessGroup(cmd.Process)

	m.mu.Lock()
	agentMap, exists := m.sessions[agentID]
	if !exists {
		agentMap = make(map[SessionID]*TerminalSession)
		m.sessions[agentID] = agentMap
	}
	tabIdx := len(agentMap) + 1
	seqNum := m.seq.Add(1)
	sessID := SessionID(fmt.Sprintf("sess-%d", seqNum))

	displayAgent := strings.TrimSpace(agentName)
	if displayAgent == "" {
		displayAgent = string(agentID)
	}
	if strings.TrimSpace(tabName) == "" {
		tabName = fmt.Sprintf("[%s:%d] %s", displayAgent, tabIdx, shellInfo.Name)
	} else {
		tabName = fmt.Sprintf("[%s:%d] %s (%s)", displayAgent, tabIdx, shellInfo.Name, strings.TrimSpace(tabName))
	}

	now := time.Now()
	ts := &TerminalSession{
		info: bus.TerminalSession{
			ID:            sessID,
			AgentID:       agentID,
			AgentName:     displayAgent,
			Name:          tabName,
			Shell:         shellInfo.Name,
			WorkspaceRoot: wsAbs,
			RelCWD:        ".",
			State:         StateIdle,
			CreatedAt:     now,
			LastOutputAt:  now,
		},
		ptmx:      ptmx,
		cmd:       cmd,
		procGroup: pg,
		ring:      NewRingBuffer(DefaultRingBufferCapacity),
		notifyCh:  make(chan struct{}, 1),
		closedCh:  make(chan struct{}),
	}
	agentMap[sessID] = ts
	m.mu.Unlock()

	go m.readLoop(ts)
	go m.waitLoop(ts)

	snap := ts.Snapshot()
	if m.eventBus != nil {
		m.eventBus.Publish(bus.Event{
			Type:            bus.EventSessionOpened,
			Timestamp:       now,
			TerminalSession: &snap,
		})
	}

	return snap, nil
}

func (m *Manager) readLoop(ts *TerminalSession) {
	buf := make([]byte, 4096)
	for {
		n, err := ts.ptmx.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			_, _ = ts.ring.Write(buf[:n])
			m.dirtyFlag.Store(true)

			now := time.Now()
			var stateChanged bool
			var snap bus.TerminalSession

			ts.mu.Lock()
			ts.info.LastOutputAt = now
			// Check if an active background marker completed after WAITING_FOR_INPUT
			if ts.activeMarker != "" {
				if code, ok := parseSentinelExitCode(chunk, ts.activeMarker); ok {
					ts.info.State = StateIdle
					ts.info.ActiveCommand = ""
					ts.info.ExitCode = &code
					if code == 0 && ts.pendingNextCD != "" {
						ts.info.RelCWD = ts.pendingNextCD
					}
					ts.activeMarker = ""
					ts.pendingNextCD = ""
					stateChanged = true
				} else if ts.info.State == StateWaitingForInput {
					ts.info.State = StateRunning
					stateChanged = true
				}
			}
			snap = ts.info
			ts.mu.Unlock()

			select {
			case ts.notifyCh <- struct{}{}:
			default:
			}

			if m.eventBus != nil {
				cleanChunk := StripANSI(chunk)
				if cleanChunk != "" {
					snapCopy := snap
					m.eventBus.Publish(bus.Event{
						Type:            bus.EventSessionOutput,
						Timestamp:       now,
						TerminalSession: &snapCopy,
						OutputChunk:     cleanChunk,
					})
				}
				if stateChanged {
					snapCopy := snap
					m.eventBus.Publish(bus.Event{
						Type:            bus.EventSessionUpdated,
						Timestamp:       now,
						TerminalSession: &snapCopy,
					})
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// PTY closed or process exited
			}
			return
		}
	}
}

func (m *Manager) waitLoop(ts *TerminalSession) {
	_ = ts.cmd.Wait()
	exitCode := 0
	if ts.cmd.ProcessState != nil {
		exitCode = ts.cmd.ProcessState.ExitCode()
	}

	ts.mu.Lock()
	ts.info.State = StateClosed
	ts.info.ActiveCommand = ""
	ts.info.ExitCode = &exitCode
	snap := ts.info
	ts.mu.Unlock()

	ts.closeOnce.Do(func() {
		close(ts.closedCh)
		_ = ts.ptmx.Close()
	})

	select {
	case ts.notifyCh <- struct{}{}:
	default:
	}

	if m.eventBus != nil {
		m.eventBus.Publish(bus.Event{
			Type:            bus.EventSessionUpdated,
			Timestamp:       time.Now(),
			TerminalSession: &snap,
		})
	}
}

// GetSession retrieves a session snapshot strictly scoped to agentID.
func (m *Manager) GetSession(agentID AgentID, sessionID SessionID) (bus.TerminalSession, error) {
	ts, err := m.lookupSession(agentID, sessionID)
	if err != nil {
		return bus.TerminalSession{}, err
	}
	return ts.Snapshot(), nil
}

// ListSessions returns all sessions belonging strictly to agentID.
func (m *Manager) ListSessions(agentID AgentID) []bus.TerminalSession {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agentMap, ok := m.sessions[agentID]
	if !ok || len(agentMap) == 0 {
		return nil
	}

	out := make([]bus.TerminalSession, 0, len(agentMap))
	for _, ts := range agentMap {
		out = append(out, ts.Snapshot())
	}
	return out
}

// AllSessions returns snapshots of all sessions across all connected agents (for the TUI).
func (m *Manager) AllSessions() []bus.TerminalSession {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []bus.TerminalSession
	for _, agentMap := range m.sessions {
		for _, ts := range agentMap {
			out = append(out, ts.Snapshot())
		}
	}
	return out
}

// Exec runs a validated command inside the specified agent's PTY session, capturing clean
// ANSI-stripped output and exit code, or reporting WAITING_FOR_INPUT if idle timeout triggers.
func (m *Manager) Exec(
	ctx context.Context,
	agentID AgentID,
	sessionID SessionID,
	command string,
	nextRelCWD string,
	timeout time.Duration,
) (ExecResult, error) {
	ts, err := m.lookupSession(agentID, sessionID)
	if err != nil {
		return ExecResult{}, err
	}

	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}

	ts.execMu.Lock()
	defer ts.execMu.Unlock()

	select {
	case <-ts.closedCh:
		return ExecResult{}, ErrSessionClosed
	default:
	}

	markerID := generateMarkerID()
	markerPrefix := "__TETHER_DONE_" + markerID + "__:"

	now := time.Now()
	ts.mu.Lock()
	if ts.info.State == StateClosed {
		ts.mu.Unlock()
		return ExecResult{}, ErrSessionClosed
	}
	shellName := ts.info.Shell
	ts.info.State = StateRunning
	ts.info.ActiveCommand = command
	ts.info.LastOutputAt = now
	ts.activeMarker = markerID
	ts.pendingNextCD = nextRelCWD
	snapRunning := ts.info
	ts.mu.Unlock()

	if m.eventBus != nil {
		m.eventBus.Publish(bus.Event{
			Type:            bus.EventSessionUpdated,
			Timestamp:       now,
			TerminalSession: &snapRunning,
		})
	}

	startOffset := ts.ring.TotalWritten()
	wrappedCmd := formatCommandWithSentinel(shellName, command, markerID)

	if _, err := io.WriteString(ts.ptmx, wrappedCmd); err != nil {
		ts.mu.Lock()
		ts.info.State = StateIdle
		ts.info.ActiveCommand = ""
		ts.activeMarker = ""
		ts.mu.Unlock()
		return ExecResult{}, fmt.Errorf("writing command to pty: %w", err)
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	idleTicker := time.NewTicker(100 * time.Millisecond)
	defer idleTicker.Stop()

	for {
		rawSince := ts.ring.ReadSince(startOffset)
		if exitCode, ok := parseSentinelExitCode(rawSince, markerID); ok {
			cleanOut := cleanExecOutput(rawSince, command, markerPrefix)

			ts.mu.Lock()
			ts.info.State = StateIdle
			ts.info.ActiveCommand = ""
			ts.info.ExitCode = &exitCode
			if exitCode == 0 && nextRelCWD != "" {
				ts.info.RelCWD = nextRelCWD
			}
			relCWD := ts.info.RelCWD
			ts.activeMarker = ""
			ts.pendingNextCD = ""
			snapDone := ts.info
			ts.mu.Unlock()

			if m.eventBus != nil {
				m.eventBus.Publish(bus.Event{
					Type:            bus.EventSessionUpdated,
					Timestamp:       time.Now(),
					TerminalSession: &snapDone,
				})
			}

			return ExecResult{
				SessionID:       sessionID,
				Output:          cleanOut,
				ExitCode:        exitCode,
				State:           StateIdle,
				RelCWD:          relCWD,
				WaitingForInput: false,
			}, nil
		}

		select {
		case <-ts.notifyCh:
			// New output arrived; loop and re-check for sentinel
		case <-idleTicker.C:
			ts.mu.RLock()
			lastOut := ts.info.LastOutputAt
			relCWD := ts.info.RelCWD
			ts.mu.RUnlock()

			if time.Since(lastOut) >= m.idleTimeout {
				// Command has stopped emitting bytes and has not finished -> WAITING_FOR_INPUT
				ts.mu.Lock()
				ts.info.State = StateWaitingForInput
				snapWait := ts.info
				ts.mu.Unlock()

				if m.eventBus != nil {
					m.eventBus.Publish(bus.Event{
						Type:            bus.EventSessionUpdated,
						Timestamp:       time.Now(),
						TerminalSession: &snapWait,
					})
				}

				rawSoFar := ts.ring.ReadSince(startOffset)
				return ExecResult{
					SessionID:       sessionID,
					Output:          cleanExecOutput(rawSoFar, command, markerPrefix),
					ExitCode:        -1,
					State:           StateWaitingForInput,
					RelCWD:          relCWD,
					WaitingForInput: true,
				}, nil
			}
		case <-ts.closedCh:
			rawSoFar := ts.ring.ReadSince(startOffset)
			snap := ts.Snapshot()
			code := -1
			if snap.ExitCode != nil {
				code = *snap.ExitCode
			}
			return ExecResult{
				SessionID: sessionID,
				Output:    cleanExecOutput(rawSoFar, command, markerPrefix),
				ExitCode:  code,
				State:     StateClosed,
				RelCWD:    snap.RelCWD,
			}, nil
		case <-execCtx.Done():
			// Send Ctrl+C to interrupt timed-out command
			_, _ = ts.ptmx.Write([]byte{0x03})
			rawSoFar := ts.ring.ReadSince(startOffset)

			ts.mu.Lock()
			ts.info.State = StateIdle
			ts.info.ActiveCommand = ""
			ts.activeMarker = ""
			ts.pendingNextCD = ""
			relCWD := ts.info.RelCWD
			snapTimeout := ts.info
			ts.mu.Unlock()

			if m.eventBus != nil {
				m.eventBus.Publish(bus.Event{
					Type:            bus.EventSessionUpdated,
					Timestamp:       time.Now(),
					TerminalSession: &snapTimeout,
				})
			}

			return ExecResult{
				SessionID: sessionID,
				Output:    cleanExecOutput(rawSoFar, command, markerPrefix),
				ExitCode:  -1,
				State:     StateIdle,
				RelCWD:    relCWD,
				TimedOut:  true,
			}, execCtx.Err()
		}
	}
}

// SendInput writes raw text or control signals (e.g. "Ctrl+C") to an active session
// strictly belonging to agentID.
func (m *Manager) SendInput(agentID AgentID, sessionID SessionID, text string, pressEnter bool) (string, bus.TerminalSession, error) {
	ts, err := m.lookupSession(agentID, sessionID)
	if err != nil {
		return "", bus.TerminalSession{}, err
	}

	select {
	case <-ts.closedCh:
		return "", bus.TerminalSession{}, ErrSessionClosed
	default:
	}

	startOffset := ts.ring.TotalWritten()
	payload := translateInputPayload(ts.Snapshot().Shell, text, pressEnter)

	if _, err := io.WriteString(ts.ptmx, payload); err != nil {
		return "", bus.TerminalSession{}, fmt.Errorf("writing input to pty: %w", err)
	}

	// Wait briefly for the process to react and emit output.
	deadline := time.After(350 * time.Millisecond)
waitLoop:
	for {
		select {
		case <-ts.notifyCh:
			ts.mu.RLock()
			st := ts.info.State
			ts.mu.RUnlock()
			if st == StateIdle {
				break waitLoop
			}
		case <-deadline:
			break waitLoop
		case <-ts.closedCh:
			break waitLoop
		}
	}

	out := StripANSI(ts.ring.ReadSince(startOffset))
	return strings.TrimSpace(out), ts.Snapshot(), nil
}

// WriteHumanInput allows the human supervisor in the TUI to send keystrokes directly
// to a session tab.
func (m *Manager) WriteHumanInput(agentID AgentID, sessionID SessionID, text string, pressEnter bool) error {
	_, _, err := m.SendInput(agentID, sessionID, text, pressEnter)
	return err
}

// WriteRaw writes raw keystroke bytes directly to the PTY without blocking,
// used in Neovim-style TERMINAL/INSERT mode.
func (m *Manager) WriteRaw(agentID AgentID, sessionID SessionID, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	ts, err := m.lookupSession(agentID, sessionID)
	if err != nil {
		return err
	}
	select {
	case <-ts.closedCh:
		return ErrSessionClosed
	default:
	}
	_, err = ts.ptmx.Write(data)
	m.dirtyFlag.Store(true)
	return err
}

// Screen returns the last n clean lines of a session strictly belonging to agentID.
func (m *Manager) Screen(agentID AgentID, sessionID SessionID, lines int) ([]string, bus.TerminalSession, error) {
	ts, err := m.lookupSession(agentID, sessionID)
	if err != nil {
		return nil, bus.TerminalSession{}, err
	}
	return ts.Screen(lines), ts.Snapshot(), nil
}

// RawScreenForTUI returns the clean text buffer of a session for rendering in the TUI.
func (m *Manager) RawScreenForTUI(agentID AgentID, sessionID SessionID) string {
	ts, err := m.lookupSession(agentID, sessionID)
	if err != nil {
		return ""
	}
	return ts.RawScreen()
}

// CloseSession terminates the entire process tree (Process Group / Job Object),
// closes the PTY, and removes the session from the agent's namespace.
func (m *Manager) CloseSession(agentID AgentID, sessionID SessionID) error {
	m.mu.Lock()
	agentMap, ok := m.sessions[agentID]
	if !ok {
		m.mu.Unlock()
		return ErrSessionNotFound
	}
	ts, exists := agentMap[sessionID]
	if !exists {
		m.mu.Unlock()
		return ErrSessionNotFound
	}
	delete(agentMap, sessionID)
	if len(agentMap) == 0 {
		delete(m.sessions, agentID)
	}
	m.mu.Unlock()

	ts.closeOnce.Do(func() {
		close(ts.closedCh)
		if ts.cmd != nil {
			killProcessTree(ts.cmd.Process, ts.procGroup)
		}
		if ts.ptmx != nil {
			_ = ts.ptmx.Close()
		}
	})

	ts.mu.Lock()
	ts.info.State = StateClosed
	ts.info.ActiveCommand = ""
	snap := ts.info
	ts.mu.Unlock()

	if m.eventBus != nil {
		m.eventBus.Publish(bus.Event{
			Type:            bus.EventSessionClosed,
			Timestamp:       time.Now(),
			TerminalSession: &snap,
		})
	}
	return nil
}

// CloseAll terminates all active PTY sessions across all agents on server shutdown.
func (m *Manager) CloseAll() {
	m.mu.RLock()
	type pair struct {
		agentID   AgentID
		sessionID SessionID
	}
	var all []pair
	for aID, sMap := range m.sessions {
		for sID := range sMap {
			all = append(all, pair{agentID: aID, sessionID: sID})
		}
	}
	m.mu.RUnlock()

	for _, p := range all {
		_ = m.CloseSession(p.agentID, p.sessionID)
	}
}

func (m *Manager) lookupSession(agentID AgentID, sessionID SessionID) (*TerminalSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agentMap, ok := m.sessions[agentID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	ts, exists := agentMap[sessionID]
	if !exists {
		return nil, ErrSessionNotFound
	}
	return ts, nil
}

func buildNonInteractiveEnv(base []string) []string {
	overrides := map[string]string{
		"CI":              "true",
		"GIT_EDITOR":      "true",
		"GIT_TERMINAL_PROMPT": "0",
		"DEBIAN_FRONTEND": "noninteractive",
		"TERM":            "xterm-256color",
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		k, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, isOverride := overrides[strings.ToUpper(k)]; isOverride {
			continue
		}
		out = append(out, entry)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func formatCommandWithSentinel(shellName, command, markerID string) string {
	shLower := strings.ToLower(shellName)
	switch {
	case strings.Contains(shLower, "pwsh") || strings.Contains(shLower, "powershell"):
		return fmt.Sprintf(
			"%s\r\n$__tether_ec = if ($LASTEXITCODE -ne $null) { [int]$LASTEXITCODE } elseif ($?) { 0 } else { 1 }; Write-Output \"__TETHER_DONE_%s__:${__tether_ec}__\"\r\n",
			command,
			markerID,
		)
	case strings.Contains(shLower, "cmd"):
		return fmt.Sprintf(
			"%s & call echo __TETHER_DONE_%s__:%%ERRORLEVEL%%__\r\n",
			command,
			markerID,
		)
	default:
		return fmt.Sprintf(
			"%s\nprintf \"\\n__TETHER_DONE_%s__:%%d__\\n\" \"$?\"\n",
			command,
			markerID,
		)
	}
}

func parseSentinelExitCode(rawOutput, markerID string) (int, bool) {
	clean := ansiRegex.ReplaceAllString(rawOutput, "")
	// Match the concrete sentinel output where the exit code is an integer (not the echoed command template with %d or ${__tether_ec})
	pattern := fmt.Sprintf(`__TETHER_DONE_%s__:(-?\d+)__`, regexp.QuoteMeta(markerID))
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(clean)
	if len(matches) < 2 {
		return 0, false
	}
	code, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

func cleanExecOutput(rawOutput, command, markerPrefix string) string {
	clean := StripANSI(rawOutput)
	lines := strings.Split(clean, "\n")
	filtered := make([]string, 0, len(lines))
	cmdTrim := strings.TrimSpace(command)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Skip echoed sentinel command fragments or prompts containing __TETHER_DONE_
		if strings.Contains(trimmed, "__TETHER_DONE_") || strings.Contains(trimmed, "$__tether_ec") || strings.Contains(trimmed, markerPrefix) {
			continue
		}
		// Skip initial echoed command line if the shell echoed it verbatim
		if len(filtered) == 0 && cmdTrim != "" && (trimmed == cmdTrim || strings.HasSuffix(trimmed, cmdTrim)) {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func translateInputPayload(shellName, text string, pressEnter bool) string {
	trimmed := strings.TrimSpace(strings.ToUpper(text))
	switch trimmed {
	case "CTRL+C", "^C", "SIGINT":
		return "\x03"
	case "CTRL+D", "^D", "EOF":
		return "\x04"
	case "CTRL+Z", "^Z":
		return "\x1a"
	}

	if !pressEnter {
		return text
	}
	shLower := strings.ToLower(shellName)
	if strings.Contains(shLower, "cmd") || strings.Contains(shLower, "pwsh") || strings.Contains(shLower, "powershell") {
		return text + "\r\n"
	}
	return text + "\n"
}

func generateMarkerID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
