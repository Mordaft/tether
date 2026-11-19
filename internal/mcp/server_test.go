package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tether/internal/auth"
	"tether/internal/bus"
	"tether/internal/env"
	"tether/internal/guard"
	"tether/internal/pty"
)

func TestMCPServer_EndToEndToolsAndResources(t *testing.T) {
	eb := bus.NewEventBus()
	sysInfo := env.Detect()
	authMgr, err := auth.NewManager(eb, 30*time.Minute)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	cmdGuard := guard.NewGuard(eb)
	ptyMgr := pty.NewManager(sysInfo, eb)
	defer ptyMgr.CloseAll()

	mcpSrv := NewServer(authMgr, cmdGuard, ptyMgr, eb, "")
	defer mcpSrv.Close()

	handler := mcpSrv.Handler()

	// 1. Unauthenticated request to /mcp must return 401 with CORS headers
	unauthReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	unauthReq.Header.Set("Origin", "https://mordaft.github.io")
	unauthRec := httptest.NewRecorder()
	handler.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized on unauthenticated /mcp request, got %d", unauthRec.Code)
	}
	if got := unauthRec.Header().Get("Access-Control-Allow-Origin"); got != "https://mordaft.github.io" {
		t.Fatalf("expected CORS Access-Control-Allow-Origin on 401 /mcp response, got %q", got)
	}
	if got := unauthRec.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Fatalf("expected Access-Control-Allow-Private-Network=true on /mcp response, got %q", got)
	}

	// 1b. OPTIONS preflight on /, /mcp, /sse, /auth/connect must return 204 No Content with CORS + PNA headers
	for _, p := range []string{"/", "/mcp", "/sse", "/auth/connect"} {
		preReq := httptest.NewRequest(http.MethodOptions, p, nil)
		preReq.Header.Set("Origin", "https://mordaft.github.io")
		preReq.Header.Set("Access-Control-Request-Method", "POST")
		preReq.Header.Set("Access-Control-Request-Private-Network", "true")
		preRec := httptest.NewRecorder()
		handler.ServeHTTP(preRec, preReq)
		if preRec.Code != http.StatusNoContent {
			t.Fatalf("expected 204 No Content on OPTIONS %s, got %d", p, preRec.Code)
		}
		if got := preRec.Header().Get("Access-Control-Allow-Origin"); got != "https://mordaft.github.io" {
			t.Fatalf("expected Access-Control-Allow-Origin on OPTIONS %s, got %q", p, got)
		}
	}

	// 2. Issue token for agent-mcp-01
	wsDir := t.TempDir()
	agentSess, err := authMgr.IssueToken("agent-mcp-01", "MCP Tester", wsDir)
	if err != nil {
		t.Fatalf("IssueToken failed: %v", err)
	}

	callRPC := func(method string, params any) map[string]any {
		t.Helper()
		reqBody, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  method,
			"params":  params,
		})
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+agentSess.AccessToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("RPC %s returned status %d: %s", method, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("failed to decode RPC response (%s): %v", rec.Body.String(), err)
		}
		return out
	}

	// 3. Initialize MCP session (both on /mcp and on root /)
	_ = callRPC("initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test-client", "version": "1.0.0"},
	})
	rootInitReq := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"root-client","version":"1.0.0"}}}`))
	rootInitReq.Header.Set("Content-Type", "application/json")
	rootInitReq.Header.Set("Authorization", "Bearer "+agentSess.AccessToken)
	rootInitReq.Header.Set("Origin", "https://mordaft.github.io")
	rootInitRec := httptest.NewRecorder()
	handler.ServeHTTP(rootInitRec, rootInitReq)
	if rootInitRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on root POST / initialize, got %d: %s", rootInitRec.Code, rootInitRec.Body.String())
	}
	if got := rootInitRec.Header().Get("Access-Control-Allow-Origin"); got != "https://mordaft.github.io" {
		t.Fatalf("expected CORS header on root POST /, got %q", got)
	}

	// 3b. Discover published tools via tools/list and resources via resources/list
	toolsList := callRPC("tools/list", map[string]any{})
	toolsBytes, _ := json.Marshal(toolsList)
	for _, toolName := range []string{ToolSessionOpen, ToolSessionExec, ToolSessionInput, ToolSessionScreen, ToolSessionClose} {
		if !strings.Contains(string(toolsBytes), toolName) {
			t.Fatalf("expected tools/list to publish %s, got %s", toolName, string(toolsBytes))
		}
	}

	resList := callRPC("resources/list", map[string]any{})
	resBytes, _ := json.Marshal(resList)
	for _, resURI := range []string{ResourceSystemURI, ResourceSessionsURI} {
		if !strings.Contains(string(resBytes), resURI) {
			t.Fatalf("expected resources/list to publish %s, got %s", resURI, string(resBytes))
		}
	}

	// 4. Read tether://env/system resource
	sysRes := callRPC("resources/read", map[string]any{
		"uri": ResourceSystemURI,
	})
	rawSys, _ := json.Marshal(sysRes)
	if !strings.Contains(string(rawSys), "agent-mcp-01") {
		t.Fatalf("expected tether://env/system to include agent-mcp-01, got %s", string(rawSys))
	}

	// 5. Open a PTY session via session_open
	requestedShell := ""
	for _, s := range sysInfo.Shells {
		if s.Name == "cmd.exe" || s.Name == "sh" || s.Name == "bash" {
			requestedShell = s.Name
			break
		}
	}
	openRes := callRPC("tools/call", map[string]any{
		"name": ToolSessionOpen,
		"arguments": map[string]any{
			"shell": requestedShell,
			"name":  "worker",
		},
	})
	openBytes, _ := json.Marshal(openRes)
	if !strings.Contains(string(openBytes), "sess-") {
		t.Fatalf("expected session_open to return session_id, got %s", string(openBytes))
	}

	sessions := ptyMgr.ListSessions("agent-mcp-01")
	if len(sessions) != 1 {
		t.Fatalf("expected 1 active session for agent-mcp-01, got %d", len(sessions))
	}
	sessID := string(sessions[0].ID)

	// 6. Execute a safe command via session_exec
	execRes := callRPC("tools/call", map[string]any{
		"name": ToolSessionExec,
		"arguments": map[string]any{
			"session_id": sessID,
			"command":    "echo MCP_EXEC_SUCCESS",
		},
	})
	execBytes, _ := json.Marshal(execRes)
	if !strings.Contains(string(execBytes), "MCP_EXEC_SUCCESS") {
		t.Fatalf("expected session_exec output to contain MCP_EXEC_SUCCESS, got %s", string(execBytes))
	}

	// 7. Execute a forbidden path traversal command via session_exec -> must be blocked by Command Guard!
	evilRes := callRPC("tools/call", map[string]any{
		"name": ToolSessionExec,
		"arguments": map[string]any{
			"session_id": sessID,
			"command":    "cat ../outside.txt",
		},
	})
	evilBytes, _ := json.Marshal(evilRes)
	if !strings.Contains(string(evilBytes), "PATH_TRAVERSAL") || !strings.Contains(string(evilBytes), `"isError":true`) {
		t.Fatalf("expected Command Guard PATH_TRAVERSAL error on '../outside.txt', got %s", string(evilBytes))
	}

	// 8. Close session via session_close
	closeRes := callRPC("tools/call", map[string]any{
		"name": ToolSessionClose,
		"arguments": map[string]any{
			"session_id": sessID,
		},
	})
	closeBytes, _ := json.Marshal(closeRes)
	if !strings.Contains(string(closeBytes), `\"closed\": true`) && !strings.Contains(string(closeBytes), `"closed":true`) {
		t.Fatalf("expected session_close to report closed:true, got %s", string(closeBytes))
	}
}
