package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tether/internal/bus"
)

// Type aliases for ergonomic access from the auth package.
type (
	AgentID      = bus.AgentID
	AgentSession = bus.AgentSession
)

var (
	ErrMissingToken   = errors.New("missing or malformed Authorization Bearer token")
	ErrInvalidToken   = errors.New("invalid or unrecognized access token")
	ErrSignature      = errors.New("invalid token cryptographic signature")
	ErrTokenExpired   = errors.New("access token has expired; renew human consent via POST /auth/connect")
	ErrImmutableClaim = errors.New("token claims mismatch immutable session binding")
)

type contextKey struct{}

var sessionContextKey = contextKey{}

// WithSession injects an authenticated AgentSession into ctx.
func WithSession(ctx context.Context, session *AgentSession) context.Context {
	return context.WithValue(ctx, sessionContextKey, session)
}

// SessionFromContext extracts the authenticated AgentSession from ctx.
func SessionFromContext(ctx context.Context) (*AgentSession, bool) {
	sess, ok := ctx.Value(sessionContextKey).(*AgentSession)
	return sess, ok && sess != nil
}

// TokenClaims represents the signed payload embedded in an access token.
type TokenClaims struct {
	TokenID       string  `json:"jti"`
	AgentID       AgentID `json:"agent_id"`
	AgentName     string  `json:"agent_name"`
	WorkspaceRoot string  `json:"workspace_root"`
	IssuedAtUnix  int64   `json:"iat"`
	ExpiresAtUnix int64   `json:"exp"`
}

// ConnectRequest is the JSON payload sent by an agent to POST /auth/connect.
type ConnectRequest struct {
	AgentID       string `json:"agent_id"`
	AgentName     string `json:"agent_name"`
	Name          string `json:"name,omitempty"`
	Workspace     string `json:"workspace"`
	WorkspaceRoot string `json:"workspace_root,omitempty"`
}

// ConnectResponse is the JSON payload returned upon human approval of POST /auth/connect.
type ConnectResponse struct {
	AccessToken   string    `json:"access_token"`
	TokenType     string    `json:"token_type"`
	AgentID       AgentID   `json:"agent_id"`
	AgentName     string    `json:"agent_name"`
	WorkspaceRoot string    `json:"workspace_root"`
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	TTLSeconds    int64     `json:"ttl_seconds"`
}

// Option configures a Manager instance.
type Option func(*Manager)

// WithClock overrides the time source (useful for deterministic TTL tests).
func WithClock(nowFunc func() time.Time) Option {
	return func(m *Manager) {
		if nowFunc != nil {
			m.nowFunc = nowFunc
		}
	}
}

// WithSecret overrides the random 256-bit HMAC key with a fixed secret (for testing).
func WithSecret(secret []byte) Option {
	return func(m *Manager) {
		if len(secret) > 0 {
			cp := make([]byte, len(secret))
			copy(cp, secret)
			m.secret = cp
		}
	}
}

// Manager handles multi-tenant agent handshakes, explicit TUI consent blocking,
// ephemeral HMAC-signed tokens with TTL, and HTTP authentication middleware.
type Manager struct {
	secret   []byte
	ttl      time.Duration
	eventBus *bus.EventBus
	nowFunc  func() time.Time

	mu       sync.RWMutex
	sessions map[string]*AgentSession // keyed by TokenID (jti)
}

// NewManager initializes an authentication Manager with an ephemeral in-memory secret key.
func NewManager(eventBus *bus.EventBus, ttl time.Duration, opts ...Option) (*Manager, error) {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generating hmac secret: %w", err)
	}

	m := &Manager{
		secret:   secret,
		ttl:      ttl,
		eventBus: eventBus,
		nowFunc:  time.Now,
		sessions: make(map[string]*AgentSession),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// IssueToken creates, signs, and stores an ephemeral AgentSession bound immutably
// to (agentID, workspaceRoot) with the manager's configured TTL.
func (m *Manager) IssueToken(agentID AgentID, agentName, workspaceRoot string) (*AgentSession, error) {
	idStr := strings.TrimSpace(string(agentID))
	if idStr == "" {
		return nil, errors.New("agent_id is required")
	}
	wsClean := normalizeWorkspaceRoot(workspaceRoot)
	if wsClean == "" {
		return nil, errors.New("workspace is required")
	}
	if strings.TrimSpace(agentName) == "" {
		agentName = idStr
	}

	var jtiBuf [16]byte
	if _, err := rand.Read(jtiBuf[:]); err != nil {
		return nil, fmt.Errorf("generating token id: %w", err)
	}
	tokenID := hex.EncodeToString(jtiBuf[:])

	now := m.nowFunc().UTC()
	exp := now.Add(m.ttl)

	claims := TokenClaims{
		TokenID:       tokenID,
		AgentID:       AgentID(idStr),
		AgentName:     agentName,
		WorkspaceRoot: wsClean,
		IssuedAtUnix:  now.UnixNano(),
		ExpiresAtUnix: exp.UnixNano(),
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("marshaling token claims: %w", err)
	}

	payloadB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	sig := m.computeHMAC(payloadB64)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	fullToken := payloadB64 + "." + sigB64

	session := bus.NewAgentSession(tokenID, AgentID(idStr), agentName, wsClean, fullToken, now, exp)

	m.mu.Lock()
	m.sessions[tokenID] = session
	m.mu.Unlock()

	return session, nil
}

// ValidateToken verifies the token signature, checks its presence in the ephemeral
// in-memory store, enforces the TTL, and verifies the immutable (agent_id, workspace_root) binding.
func (m *Manager) ValidateToken(rawToken string) (*AgentSession, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return nil, ErrMissingToken
	}

	parts := strings.Split(rawToken, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, ErrInvalidToken
	}

	payloadB64, sigB64 := parts[0], parts[1]
	gotSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, ErrInvalidToken
	}

	expectedSig := m.computeHMAC(payloadB64)
	if !hmac.Equal(gotSig, expectedSig) {
		return nil, ErrSignature
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, ErrInvalidToken
	}

	var claims TokenClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, ErrInvalidToken
	}

	now := m.nowFunc().UTC()
	expTime := time.Unix(0, claims.ExpiresAtUnix).UTC()

	m.mu.RLock()
	session, exists := m.sessions[claims.TokenID]
	m.mu.RUnlock()

	if !exists {
		if !now.Before(expTime) {
			return nil, ErrTokenExpired
		}
		return nil, ErrInvalidToken
	}

	if session.IsExpired(now) || !now.Before(expTime) {
		m.mu.Lock()
		delete(m.sessions, claims.TokenID)
		m.mu.Unlock()

		if m.eventBus != nil {
			m.eventBus.Publish(bus.Event{
				Type:         bus.EventAgentExpired,
				Timestamp:    now,
				AgentSession: session,
				Message:      fmt.Sprintf("Session expired for agent %s", session.AgentID),
			})
		}
		return nil, ErrTokenExpired
	}

	if session.AgentID != claims.AgentID || session.WorkspaceRoot != claims.WorkspaceRoot {
		return nil, ErrImmutableClaim
	}

	return session, nil
}

// RevokeToken invalidates an active token immediately.
func (m *Manager) RevokeToken(tokenID string) {
	m.mu.Lock()
	sess, ok := m.sessions[tokenID]
	if ok {
		delete(m.sessions, tokenID)
	}
	m.mu.Unlock()

	if ok && m.eventBus != nil {
		m.eventBus.Publish(bus.Event{
			Type:         bus.EventAgentRevoked,
			Timestamp:    m.nowFunc().UTC(),
			AgentSession: sess,
		})
	}
}

// ActiveSessions returns all currently valid (non-expired) agent sessions.
func (m *Manager) ActiveSessions() []*AgentSession {
	now := m.nowFunc().UTC()
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*AgentSession, 0, len(m.sessions))
	for _, sess := range m.sessions {
		if !sess.IsExpired(now) {
			out = append(out, sess)
		}
	}
	return out
}

// HandleConnect implements the POST /auth/connect handshake endpoint.
// It blocks awaiting human approval on the TUI consent queue before issuing an access token.
func (m *Manager) HandleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed; use POST /auth/connect")
		return
	}

	var req ConnectRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	agentID := strings.TrimSpace(req.AgentID)
	agentName := strings.TrimSpace(req.AgentName)
	if agentName == "" {
		agentName = strings.TrimSpace(req.Name)
	}
	if agentName == "" {
		agentName = agentID
	}

	workspace := strings.TrimSpace(req.Workspace)
	if workspace == "" {
		workspace = strings.TrimSpace(req.WorkspaceRoot)
	}

	if agentID == "" {
		writeJSONError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	if workspace == "" {
		writeJSONError(w, http.StatusBadRequest, "workspace is required")
		return
	}

	cleanWorkspace := normalizeWorkspaceRoot(workspace)

	if m.eventBus == nil {
		writeJSONError(w, http.StatusInternalServerError, "consent bus unavailable")
		return
	}

	consentReq := bus.ConsentRequest{
		Kind:      bus.ConsentKindConnect,
		AgentID:   AgentID(agentID),
		AgentName: agentName,
		Workspace: cleanWorkspace,
		Reason:    fmt.Sprintf("Agent %q (%s) requests access to workspace %s", agentName, agentID, cleanWorkspace),
	}

	consentRes, err := m.eventBus.RequestConsent(r.Context(), consentReq)
	if err != nil {
		writeJSONError(w, http.StatusRequestTimeout, "connection handshake cancelled or timed out awaiting human approval")
		return
	}

	if consentRes.Decision == bus.DecisionDeny {
		writeJSONError(w, http.StatusForbidden, "connection request denied by human operator")
		return
	}

	session, err := m.IssueToken(AgentID(agentID), agentName, cleanWorkspace)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to issue access token: "+err.Error())
		return
	}

	if consentRes.Decision == bus.DecisionAllowSession {
		session.GrantSessionConsent("*")
	}

	m.eventBus.Publish(bus.Event{
		Type:         bus.EventAgentConnected,
		Timestamp:    m.nowFunc().UTC(),
		AgentSession: session,
		Message:      fmt.Sprintf("Agent %s connected to %s", session.AgentID, session.WorkspaceRoot),
	})

	resp := ConnectResponse{
		AccessToken:   session.AccessToken,
		TokenType:     "Bearer",
		AgentID:       session.AgentID,
		AgentName:     session.AgentName,
		WorkspaceRoot: session.WorkspaceRoot,
		IssuedAt:      session.IssuedAt,
		ExpiresAt:     session.ExpiresAt,
		TTLSeconds:    int64(m.ttl.Seconds()),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// Middleware validates the Authorization: Bearer <access_token> header on incoming
// HTTP/SSE requests, enforces signature and TTL, and injects AgentSession into context.Context.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawToken, ok := extractBearerToken(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tether"`)
			writeJSONError(w, http.StatusUnauthorized, ErrMissingToken.Error())
			return
		}

		session, err := m.ValidateToken(rawToken)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tether", error="invalid_token"`)
			writeJSONError(w, http.StatusUnauthorized, err.Error())
			return
		}

		ctx := WithSession(r.Context(), session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (m *Manager) computeHMAC(payload string) []byte {
	mac := hmac.New(sha256.New, m.secret)
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func extractBearerToken(authHeader string) (string, bool) {
	authHeader = strings.TrimSpace(authHeader)
	if len(authHeader) < 7 {
		return "", false
	}
	if !strings.EqualFold(authHeader[:7], "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(authHeader[7:])
	if token == "" {
		return "", false
	}
	return token, true
}

func normalizeWorkspaceRoot(ws string) string {
	ws = strings.TrimSpace(ws)
	if ws == "" {
		return ""
	}
	return filepath.Clean(ws)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  msg,
		"status": status,
	})
}
