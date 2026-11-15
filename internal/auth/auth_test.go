package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tether/internal/bus"
)

func TestHandshakeApproveAndMiddleware(t *testing.T) {
	eb := bus.NewEventBus()
	eventsCh, unsub := eb.Subscribe(16)
	defer unsub()

	now := time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)
	mgr, err := NewManager(eb, 30*time.Minute, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	// Simulate human operator approving connection in the TUI
	go func() {
		for evt := range eventsCh {
			if evt.Type == bus.EventConsentQueued && evt.ConsentRequest.Kind == bus.ConsentKindConnect {
				_ = eb.ResolveConsent(evt.ConsentRequest.ID, bus.DecisionAllowOnce, "approved in test")
				return
			}
		}
	}()

	body, _ := json.Marshal(ConnectRequest{
		AgentID:   "localloop-coder-01",
		AgentName: "LocalLoop Coder",
		Workspace: "/projects/tether-app",
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/connect", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	mgr.HandleConnect(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on approved handshake, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ConnectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode ConnectResponse: %v", err)
	}
	if resp.AgentID != "localloop-coder-01" {
		t.Fatalf("unexpected agent_id: %s", resp.AgentID)
	}
	if resp.AccessToken == "" {
		t.Fatalf("expected non-empty access_token")
	}

	// Validate protected route with Middleware
	protected := mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := SessionFromContext(r.Context())
		if !ok {
			t.Errorf("expected AgentSession in context")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if sess.AgentID != "localloop-coder-01" {
			t.Errorf("expected agent_id localloop-coder-01, got %s", sess.AgentID)
		}
		w.WriteHeader(http.StatusOK)
	}))

	authedReq := httptest.NewRequest(http.MethodGet, "/mcp/sse", nil)
	authedReq.Header.Set("Authorization", "Bearer "+resp.AccessToken)
	authedRec := httptest.NewRecorder()
	protected.ServeHTTP(authedRec, authedReq)

	if authedRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from middleware with valid token, got %d", authedRec.Code)
	}

	// Advance clock beyond 30m TTL -> must return 401 Unauthorized
	now = now.Add(31 * time.Minute)
	expiredReq := httptest.NewRequest(http.MethodGet, "/mcp/sse", nil)
	expiredReq.Header.Set("Authorization", "Bearer "+resp.AccessToken)
	expiredRec := httptest.NewRecorder()
	protected.ServeHTTP(expiredRec, expiredReq)

	if expiredRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for expired token, got %d", expiredRec.Code)
	}
}

func TestHandshakeDeniedByHuman(t *testing.T) {
	eb := bus.NewEventBus()
	eventsCh, unsub := eb.Subscribe(16)
	defer unsub()

	mgr, err := NewManager(eb, 15*time.Minute)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	go func() {
		for evt := range eventsCh {
			if evt.Type == bus.EventConsentQueued {
				_ = eb.ResolveConsent(evt.ConsentRequest.ID, bus.DecisionDeny, "operator denied")
				return
			}
		}
	}()

	body, _ := json.Marshal(ConnectRequest{
		AgentID:   "rogue-agent",
		AgentName: "Rogue",
		Workspace: "/etc",
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/connect", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	mgr.HandleConnect(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when operator denies handshake, got %d", rec.Code)
	}
	if len(mgr.ActiveSessions()) != 0 {
		t.Fatalf("expected 0 active sessions after denied handshake")
	}
}

func TestTokenTamperingAndMissingHeader(t *testing.T) {
	eb := bus.NewEventBus()
	mgr, err := NewManager(eb, 15*time.Minute)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	sess, err := mgr.IssueToken("agent-1", "Agent One", "/workspace/one")
	if err != nil {
		t.Fatalf("IssueToken failed: %v", err)
	}

	// 1. Tamper with signature
	parts := strings.Split(sess.AccessToken, ".")
	tamperedSig := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("forged-signature-32-bytes-long!!"))
	if _, err := mgr.ValidateToken(tamperedSig); err != ErrSignature {
		t.Fatalf("expected ErrSignature on tampered signature, got %v", err)
	}

	// 2. Tamper with payload claims (attempting to change workspace_root)
	forgedClaims, _ := json.Marshal(TokenClaims{
		TokenID:       sess.TokenID,
		AgentID:       "agent-1",
		WorkspaceRoot: "/root",
		IssuedAtUnix:  sess.IssuedAt.UnixNano(),
		ExpiresAtUnix: sess.ExpiresAt.UnixNano(),
	})
	forgedToken := base64.RawURLEncoding.EncodeToString(forgedClaims) + "." + parts[1]
	if _, err := mgr.ValidateToken(forgedToken); err != ErrSignature {
		t.Fatalf("expected ErrSignature on tampered payload, got %v", err)
	}

	// 3. Missing Authorization header on middleware
	protected := mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp/message", nil)
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on missing Authorization header, got %d", rec.Code)
	}
}
