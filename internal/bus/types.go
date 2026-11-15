package bus

import (
	"net/http"
	"sync"
	"time"
)

// AgentID uniquely identifies a connected agent client (e.g. "localloop-coder-01").
type AgentID string

// SessionID uniquely identifies a PTY terminal session belonging to an agent.
type SessionID string

// SessionState represents the execution lifecycle state of a PTY terminal session.
type SessionState string

const (
	StateIdle            SessionState = "IDLE"
	StateRunning         SessionState = "RUNNING"
	StateWaitingForInput SessionState = "WAITING_FOR_INPUT"
	StateClosed          SessionState = "CLOSED"
)

// AgentSession represents an authenticated multi-tenant agent session bound
// immutably to (AgentID, WorkspaceRoot) with an explicit TTL.
type AgentSession struct {
	TokenID       string    `json:"token_id"`
	AgentID       AgentID   `json:"agent_id"`
	AgentName     string    `json:"agent_name"`
	WorkspaceRoot string    `json:"workspace_root"`
	AccessToken   string    `json:"access_token,omitempty"`
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`

	mu                  sync.RWMutex
	allowAllInSession   bool
	allowedSessionCmds  map[string]bool
}

// NewAgentSession creates an initialized AgentSession.
func NewAgentSession(tokenID string, agentID AgentID, agentName, workspaceRoot, token string, issuedAt, expiresAt time.Time) *AgentSession {
	return &AgentSession{
		TokenID:            tokenID,
		AgentID:            agentID,
		AgentName:          agentName,
		WorkspaceRoot:      workspaceRoot,
		AccessToken:        token,
		IssuedAt:           issuedAt,
		ExpiresAt:          expiresAt,
		allowedSessionCmds: make(map[string]bool),
	}
}

// IsExpired reports whether the session token has passed its expiration time at now.
func (s *AgentSession) IsExpired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}

// GrantSessionConsent records that the human operator approved commands for this session.
// If commandKey is "*", all syntactically safe commands in this session are pre-approved.
func (s *AgentSession) GrantSessionConsent(commandKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if commandKey == "*" || commandKey == "" {
		s.allowAllInSession = true
		return
	}
	if s.allowedSessionCmds == nil {
		s.allowedSessionCmds = make(map[string]bool)
	}
	s.allowedSessionCmds[commandKey] = true
}

// HasSessionConsent checks if a command or the entire session has already been
// granted "Permitir en esta sesión" approval by the user.
func (s *AgentSession) HasSessionConsent(commandKey string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.allowAllInSession {
		return true
	}
	if s.allowedSessionCmds == nil {
		return false
	}
	return s.allowedSessionCmds[commandKey]
}

// TerminalSession represents the metadata and state snapshot of an isolated PTY session.
type TerminalSession struct {
	ID            SessionID    `json:"session_id"`
	AgentID       AgentID      `json:"agent_id"`
	AgentName     string       `json:"agent_name,omitempty"`
	Name          string       `json:"name"`
	Shell         string       `json:"shell"`
	WorkspaceRoot string       `json:"workspace_root"`
	RelCWD        string       `json:"rel_cwd"`
	State         SessionState `json:"state"`
	ActiveCommand string       `json:"active_command,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	LastOutputAt  time.Time    `json:"last_output_at"`
	ExitCode      *int         `json:"exit_code,omitempty"`
}

// ConsentKind distinguishes between agent connection handshake requests and
// command execution confirmation requests in the shared TUI queue.
type ConsentKind string

const (
	ConsentKindConnect ConsentKind = "AUTH_CONNECT"
	ConsentKindCommand ConsentKind = "COMMAND_EXEC"
)

// ConsentDecision represents the operator's action in the TUI consent bar.
type ConsentDecision string

const (
	DecisionAllowOnce    ConsentDecision = "ALLOW_ONCE"    // [A]probar (Permitir una vez)
	DecisionAllowSession ConsentDecision = "ALLOW_SESSION" // [S]esión  (Permitir en esta sesión)
	DecisionDeny         ConsentDecision = "DENY"          // [D]enegar (Denegar)
)

// ConsentResult wraps the human operator's decision and an optional reason.
type ConsentResult struct {
	Decision   ConsentDecision `json:"decision"`
	Reason     string          `json:"reason,omitempty"`
	ResolvedAt time.Time       `json:"resolved_at"`
}

// ConsentRequest represents an item waiting in the centralized FIFO human approval queue.
type ConsentRequest struct {
	ID         string             `json:"id"`
	Kind       ConsentKind        `json:"kind"`
	AgentID    AgentID            `json:"agent_id"`
	AgentName  string             `json:"agent_name"`
	SessionID  SessionID          `json:"session_id,omitempty"`
	Workspace  string             `json:"workspace"`
	RelCWD     string             `json:"rel_cwd,omitempty"`
	Command    string             `json:"command,omitempty"`
	Reason     string             `json:"reason"`
	CreatedAt  time.Time          `json:"created_at"`
	ResponseCh chan ConsentResult `json:"-"`
}

// VerdictAction indicates the outcome of Command Guard validation.
type VerdictAction string

const (
	VerdictAllow        VerdictAction = "ALLOW"
	VerdictNeedsConsent VerdictAction = "NEEDS_CONSENT"
	VerdictReject       VerdictAction = "REJECT"
)

// ViolationCode categorizes why a command was rejected by the Command Guard.
type ViolationCode string

const (
	ViolationNone                ViolationCode = ""
	ViolationEmptyCommand        ViolationCode = "EMPTY_COMMAND"
	ViolationSyntaxError         ViolationCode = "SYNTAX_ERROR"
	ViolationPathTraversal       ViolationCode = "PATH_TRAVERSAL"
	ViolationAbsolutePath        ViolationCode = "ABSOLUTE_PATH"
	ViolationHomeExpansion       ViolationCode = "HOME_EXPANSION"
	ViolationCommandSubstitution ViolationCode = "COMMAND_SUBSTITUTION"
	ViolationCWDEscape           ViolationCode = "CWD_ESCAPE"
	ViolationEnvEscape           ViolationCode = "ENV_ESCAPE"
	ViolationConsentDenied       ViolationCode = "CONSENT_DENIED"
)

// GuardVerdict encapsulates the syntactic and policy verdict produced by Command Guard.
type GuardVerdict struct {
	Action     VerdictAction `json:"action"`
	HTTPStatus int           `json:"http_status"`
	Violation  ViolationCode `json:"violation,omitempty"`
	Reason     string        `json:"reason"`
	NextRelCWD string        `json:"next_rel_cwd"`
	Commands   []string      `json:"commands,omitempty"`
}

// Allowed reports whether the verdict permits immediate execution without further prompts.
func (v GuardVerdict) Allowed() bool {
	return v.Action == VerdictAllow && v.HTTPStatus == http.StatusOK
}

// EventType identifies the kind of system event broadcast over the internal EventBus.
type EventType string

const (
	EventConsentQueued    EventType = "CONSENT_QUEUED"
	EventConsentResolved  EventType = "CONSENT_RESOLVED"
	EventConsentCancelled EventType = "CONSENT_CANCELLED"
	EventAgentConnected   EventType = "AGENT_CONNECTED"
	EventAgentExpired     EventType = "AGENT_EXPIRED"
	EventAgentRevoked     EventType = "AGENT_REVOKED"
	EventSessionOpened    EventType = "SESSION_OPENED"
	EventSessionUpdated   EventType = "SESSION_UPDATED"
	EventSessionOutput    EventType = "SESSION_OUTPUT"
	EventSessionClosed    EventType = "SESSION_CLOSED"
)

// Event is the envelope published through the internal EventBus.
type Event struct {
	Type            EventType        `json:"type"`
	Timestamp       time.Time        `json:"timestamp"`
	AgentSession    *AgentSession    `json:"agent_session,omitempty"`
	TerminalSession *TerminalSession `json:"terminal_session,omitempty"`
	ConsentRequest  *ConsentRequest  `json:"consent_request,omitempty"`
	ConsentResult   *ConsentResult   `json:"consent_result,omitempty"`
	OutputChunk     string           `json:"output_chunk,omitempty"`
	Message         string           `json:"message,omitempty"`
}
