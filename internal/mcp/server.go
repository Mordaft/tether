package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"tether/internal/auth"
	"tether/internal/bus"
	"tether/internal/env"
	"tether/internal/guard"
	"tether/internal/pty"
)

const (
	ResourceSystemURI   = "tether://env/system"
	ResourceSessionsURI = "tether://sessions"

	ToolSessionOpen   = "session_open"
	ToolSessionExec   = "session_exec"
	ToolSessionInput  = "session_input"
	ToolSessionScreen = "session_screen"
	ToolSessionClose  = "session_close"
)

// Server wraps the MCP protocol server and HTTP/SSE + Streamable HTTP transports,
// completely decoupled from os.Stdin/os.Stdout so the Bubbletea TUI owns the console.
type Server struct {
	authMgr          *auth.Manager
	cmdGuard         *guard.Guard
	ptyMgr           *pty.Manager
	eventBus         *bus.EventBus
	mcpServer        *mcpserver.MCPServer
	sseServer        *mcpserver.SSEServer
	streamableServer *mcpserver.StreamableHTTPServer
	mux              *http.ServeMux
}

// NewServer constructs and wires the MCP server with auth, guard, and pty managers.
func NewServer(
	authMgr *auth.Manager,
	cmdGuard *guard.Guard,
	ptyMgr *pty.Manager,
	eventBus *bus.EventBus,
	baseURL string,
) *Server {
	discardLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mcpSrv := mcpserver.NewMCPServer(
		"tether",
		"1.0.0",
		mcpserver.WithResourceCapabilities(false, false),
		mcpserver.WithToolCapabilities(false),
	)

	s := &Server{
		authMgr:   authMgr,
		cmdGuard:  cmdGuard,
		ptyMgr:    ptyMgr,
		eventBus:  eventBus,
		mcpServer: mcpSrv,
	}

	s.registerResources()
	s.registerTools()

	sseOpts := []mcpserver.SSEOption{
		mcpserver.WithSSEEndpoint("/sse"),
		mcpserver.WithMessageEndpoint("/message"),
		mcpserver.WithUseFullURLForMessageEndpoint(false),
		mcpserver.WithSSEContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if sess, ok := auth.SessionFromContext(r.Context()); ok {
				return auth.WithSession(ctx, sess)
			}
			return ctx
		}),
	}
	if baseURL != "" {
		sseOpts = append(sseOpts, mcpserver.WithBaseURL(baseURL))
	}
	s.sseServer = mcpserver.NewSSEServer(mcpSrv, sseOpts...)

	s.streamableServer = mcpserver.NewStreamableHTTPServer(
		mcpSrv,
		mcpserver.WithEndpointPath("/mcp"),
		mcpserver.WithStateLess(true),
		mcpserver.WithStreamableHTTPLogger(discardLogger),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if sess, ok := auth.SessionFromContext(r.Context()); ok {
				return auth.WithSession(ctx, sess)
			}
			return ctx
		}),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/connect", authMgr.HandleConnect)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})

	authedSSE := authMgr.Middleware(s.sseServer)
	authedStreamable := authMgr.Middleware(s.streamableServer)

	mux.Handle("/sse", authedSSE)
	mux.Handle("/message", authedSSE)
	mux.Handle("/mcp", authedStreamable)
	mux.Handle("/mcp/", authedStreamable)

	s.mux = mux
	return s
}

// Handler returns the root HTTP handler serving /auth/connect, /sse, /message, and /mcp.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// MCPServer returns the underlying mark3labs MCPServer instance.
func (s *Server) MCPServer() *mcpserver.MCPServer {
	return s.mcpServer
}

// Close terminates active SSE connections.
func (s *Server) Close() {
	if s.sseServer != nil {
		s.sseServer.CloseSessions()
	}
}

func (s *Server) registerResources() {
	sysRes := mcp.NewResource(
		ResourceSystemURI,
		"Host System & Agent Workspace Context",
		mcp.WithResourceDescription("OS, CPU architecture, detected shells, and the authenticated agent's assigned workspace_root"),
		mcp.WithMIMEType("application/json"),
	)
	s.mcpServer.AddResource(sysRes, s.handleResourceSystem)

	sessRes := mcp.NewResource(
		ResourceSessionsURI,
		"Active Agent PTY Sessions",
		mcp.WithResourceDescription("List of active PTY terminal sessions belonging to the authenticated agent, relative CWD, and state"),
		mcp.WithMIMEType("application/json"),
	)
	s.mcpServer.AddResource(sessRes, s.handleResourceSessions)
}

func (s *Server) handleResourceSystem(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	agentSess, ok := auth.SessionFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("unauthorized: missing authenticated agent session in context")
	}

	sysInfo := s.ptyMgr.SystemInfo()
	payload := struct {
		OS            string          `json:"os"`
		Arch          string          `json:"arch"`
		DefaultShell  string          `json:"default_shell"`
		Shells        []env.ShellInfo `json:"shells"`
		AgentID       bus.AgentID     `json:"agent_id"`
		AgentName     string          `json:"agent_name"`
		WorkspaceRoot string          `json:"workspace_root"`
	}{
		OS:            sysInfo.OS,
		Arch:          sysInfo.Arch,
		DefaultShell:  sysInfo.DefaultShell,
		Shells:        sysInfo.Shells,
		AgentID:       agentSess.AgentID,
		AgentName:     agentSess.AgentName,
		WorkspaceRoot: agentSess.WorkspaceRoot,
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      ResourceSystemURI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) handleResourceSessions(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	agentSess, ok := auth.SessionFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("unauthorized: missing authenticated agent session in context")
	}

	sessions := s.ptyMgr.ListSessions(agentSess.AgentID)
	if sessions == nil {
		sessions = []bus.TerminalSession{}
	}

	payload := struct {
		AgentID       bus.AgentID           `json:"agent_id"`
		WorkspaceRoot string                `json:"workspace_root"`
		Sessions      []bus.TerminalSession `json:"sessions"`
	}{
		AgentID:       agentSess.AgentID,
		WorkspaceRoot: agentSess.WorkspaceRoot,
		Sessions:      sessions,
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      ResourceSessionsURI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func (s *Server) registerTools() {
	// 1. session_open(shell?, name?)
	openTool := mcp.NewTool(
		ToolSessionOpen,
		mcp.WithDescription("Open a new isolated PTY terminal tab in the agent's authorized workspace_root and return its session_id."),
		mcp.WithString("shell", mcp.Description("Optional shell interpreter name (e.g. bash, zsh, sh, pwsh, powershell.exe, cmd.exe)")),
		mcp.WithString("name", mcp.Description("Optional descriptive label for the terminal tab")),
	)
	s.mcpServer.AddTool(openTool, s.handleToolSessionOpen)

	// 2. session_exec(session_id, command, timeout_ms?)
	execTool := mcp.NewTool(
		ToolSessionExec,
		mcp.WithDescription("Validate command via Command Guard AST, request human consent if required, execute in the PTY session, and return clean ANSI-stripped output + exit code."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Target PTY session_id")),
		mcp.WithString("command", mcp.Required(), mcp.Description("Relative-path shell command to execute within workspace_root")),
		mcp.WithNumber("timeout_ms", mcp.Description("Optional execution timeout in milliseconds (default 30000)")),
	)
	s.mcpServer.AddTool(execTool, s.handleToolSessionExec)

	// 3. session_input(session_id, text, press_enter?)
	inputTool := mcp.NewTool(
		ToolSessionInput,
		mcp.WithDescription("Send interactive text input or control signals (e.g. 'Ctrl+C') to an active PTY session."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Target PTY session_id")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Text or signal ('Ctrl+C', 'Ctrl+D') to send")),
		mcp.WithBoolean("press_enter", mcp.Description("Whether to append Enter/newline after text (default true)")),
	)
	s.mcpServer.AddTool(inputTool, s.handleToolSessionInput)

	// 4. session_screen(session_id, lines?)
	screenTool := mcp.NewTool(
		ToolSessionScreen,
		mcp.WithDescription("Return the last N ANSI-sanitized lines from the PTY terminal ring buffer."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Target PTY session_id")),
		mcp.WithNumber("lines", mcp.Description("Number of recent lines to return (default 50)")),
	)
	s.mcpServer.AddTool(screenTool, s.handleToolSessionScreen)

	// 5. session_close(session_id)
	closeTool := mcp.NewTool(
		ToolSessionClose,
		mcp.WithDescription("Terminate the entire process tree of the PTY session and close its tab."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Target PTY session_id")),
	)
	s.mcpServer.AddTool(closeTool, s.handleToolSessionClose)
}

func (s *Server) handleToolSessionOpen(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agentSess, ok := auth.SessionFromContext(ctx)
	if !ok {
		return mcp.NewToolResultError("401 Unauthorized: missing authenticated agent session"), nil
	}

	var args struct {
		Shell string `json:"shell"`
		Name  string `json:"name"`
	}
	_ = req.BindArguments(&args)

	sess, err := s.ptyMgr.OpenSession(
		agentSess.AgentID,
		agentSess.AgentName,
		agentSess.WorkspaceRoot,
		args.Shell,
		args.Name,
	)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to open PTY session: %v", err)), nil
	}

	return jsonToolResult(sess, false)
}

func (s *Server) handleToolSessionExec(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agentSess, ok := auth.SessionFromContext(ctx)
	if !ok {
		return mcp.NewToolResultError("401 Unauthorized: missing authenticated agent session"), nil
	}

	var args struct {
		SessionID string `json:"session_id"`
		Command   string `json:"command"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	if err := req.BindArguments(&args); err != nil {
		return mcp.NewToolResultError("invalid arguments: " + err.Error()), nil
	}

	sessID := bus.SessionID(strings.TrimSpace(args.SessionID))
	if sessID == "" {
		return mcp.NewToolResultError("session_id is required"), nil
	}

	termSess, err := s.ptyMgr.GetSession(agentSess.AgentID, sessID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("403/404 session error: %v", err)), nil
	}

	// Pass command through Command Guard AST & human consent queue
	verdict, err := s.cmdGuard.Authorize(ctx, agentSess, sessID, termSess.RelCWD, args.Command)
	if err != nil {
		return jsonToolResult(map[string]any{
			"error":       err.Error(),
			"http_status": verdict.HTTPStatus,
			"violation":   verdict.Violation,
			"reason":      verdict.Reason,
		}, true)
	}

	if !verdict.Allowed() {
		return jsonToolResult(map[string]any{
			"error":       "403 Forbidden: command rejected by Command Guard",
			"http_status": verdict.HTTPStatus,
			"violation":   verdict.Violation,
			"reason":      verdict.Reason,
		}, true)
	}

	timeout := pty.DefaultExecTimeout
	if args.TimeoutMs > 0 {
		timeout = time.Duration(args.TimeoutMs) * time.Millisecond
	}

	execRes, execErr := s.ptyMgr.Exec(ctx, agentSess.AgentID, sessID, args.Command, verdict.NextRelCWD, timeout)
	if execErr != nil && !execRes.TimedOut {
		return mcp.NewToolResultError(fmt.Sprintf("execution failed: %v", execErr)), nil
	}

	return jsonToolResult(execRes, execRes.TimedOut)
}

func (s *Server) handleToolSessionInput(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agentSess, ok := auth.SessionFromContext(ctx)
	if !ok {
		return mcp.NewToolResultError("401 Unauthorized: missing authenticated agent session"), nil
	}

	rawArgs := req.GetArguments()
	pressEnter := true
	if rawArgs != nil {
		if v, exists := rawArgs["press_enter"]; exists {
			if b, ok := v.(bool); ok {
				pressEnter = b
			}
		}
	}

	sessionID := bus.SessionID(strings.TrimSpace(req.GetString("session_id", "")))
	text := req.GetString("text", "")
	if sessionID == "" {
		return mcp.NewToolResultError("session_id is required"), nil
	}

	out, snap, err := s.ptyMgr.SendInput(agentSess.AgentID, sessionID, text, pressEnter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("session_input failed: %v", err)), nil
	}

	return jsonToolResult(map[string]any{
		"session_id": snap.ID,
		"output":     out,
		"state":      snap.State,
		"rel_cwd":    snap.RelCWD,
	}, false)
}

func (s *Server) handleToolSessionScreen(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agentSess, ok := auth.SessionFromContext(ctx)
	if !ok {
		return mcp.NewToolResultError("401 Unauthorized: missing authenticated agent session"), nil
	}

	var args struct {
		SessionID string `json:"session_id"`
		Lines     int    `json:"lines"`
	}
	_ = req.BindArguments(&args)

	sessionID := bus.SessionID(strings.TrimSpace(args.SessionID))
	if sessionID == "" {
		return mcp.NewToolResultError("session_id is required"), nil
	}
	if args.Lines <= 0 {
		args.Lines = 50
	}

	lines, snap, err := s.ptyMgr.Screen(agentSess.AgentID, sessionID, args.Lines)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("session_screen failed: %v", err)), nil
	}

	return jsonToolResult(map[string]any{
		"session_id": snap.ID,
		"state":      snap.State,
		"rel_cwd":    snap.RelCWD,
		"lines":      lines,
		"screen":     strings.Join(lines, "\n"),
	}, false)
}

func (s *Server) handleToolSessionClose(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agentSess, ok := auth.SessionFromContext(ctx)
	if !ok {
		return mcp.NewToolResultError("401 Unauthorized: missing authenticated agent session"), nil
	}

	sessionID := bus.SessionID(strings.TrimSpace(req.GetString("session_id", "")))
	if sessionID == "" {
		return mcp.NewToolResultError("session_id is required"), nil
	}

	if err := s.ptyMgr.CloseSession(agentSess.AgentID, sessionID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("session_close failed: %v", err)), nil
	}

	return jsonToolResult(map[string]any{
		"session_id": sessionID,
		"closed":     true,
		"state":      bus.StateClosed,
	}, false)
}

func jsonToolResult(payload any, isError bool) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("failed to serialize result: " + err.Error()), nil
	}
	if isError {
		return mcp.NewToolResultError(string(data)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}
