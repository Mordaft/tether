package pty

import (
	"context"
	"strings"
	"testing"
	"time"

	"tether/internal/bus"
	"tether/internal/env"
)

func TestStripANSIAndRingBuffer(t *testing.T) {
	raw := "\x1b[32mSUCCESS:\x1b[0m build \x1b]0;WindowTitle\x07complete\r\nline2\n"
	clean := StripANSI(raw)
	if clean != "SUCCESS: build complete\nline2\n" {
		t.Fatalf("unexpected StripANSI output: %q", clean)
	}

	rb := NewRingBuffer(16)
	_, _ = rb.Write([]byte("0123456789"))
	off := rb.TotalWritten()
	_, _ = rb.Write([]byte("abcdefghij"))

	if got := rb.ReadSince(off); got != "abcdefghij" {
		t.Fatalf("expected 'abcdefghij' from ReadSince, got %q", got)
	}
	if len(rb.String()) != 16 {
		t.Fatalf("expected ring buffer capped at 16 bytes, got %d", len(rb.String()))
	}
}

func TestPTYManager_MultiTenantIsolationAndExec(t *testing.T) {
	sysInfo := env.Detect()
	eb := bus.NewEventBus()
	mgr := NewManager(sysInfo, eb, WithIdleTimeout(4*time.Second))
	defer mgr.CloseAll()

	wsA := t.TempDir()
	wsB := t.TempDir()

	// Prefer cmd.exe on Windows for fast startup in tests, or default shell on Unix
	requestedShell := ""
	for _, s := range sysInfo.Shells {
		if s.Name == "cmd.exe" || s.Name == "sh" || s.Name == "bash" {
			requestedShell = s.Name
			break
		}
	}

	sessA, err := mgr.OpenSession("agent-alpha", "Alpha", wsA, requestedShell, "build")
	if err != nil {
		t.Fatalf("OpenSession for agent-alpha failed: %v", err)
	}
	sessB, err := mgr.OpenSession("agent-beta", "Beta", wsB, requestedShell, "test")
	if err != nil {
		t.Fatalf("OpenSession for agent-beta failed: %v", err)
	}

	// Verify strict namespace isolation: agent-beta CANNOT see or touch agent-alpha's session
	listB := mgr.ListSessions("agent-beta")
	if len(listB) != 1 || listB[0].ID != sessB.ID {
		t.Fatalf("agent-beta should only see its own session, got %+v", listB)
	}
	if _, err := mgr.GetSession("agent-beta", sessA.ID); err != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound when agent-beta queries agent-alpha's session, got %v", err)
	}
	if _, _, err := mgr.Screen("agent-beta", sessA.ID, 10); err != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound when agent-beta reads agent-alpha's screen, got %v", err)
	}
	if _, _, err := mgr.SendInput("agent-beta", sessA.ID, "whoami", true); err != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound when agent-beta sends input to agent-alpha's session, got %v", err)
	}
	if err := mgr.CloseSession("agent-beta", sessA.ID); err != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound when agent-beta tries to close agent-alpha's session, got %v", err)
	}

	// Execute a command in agent-alpha's session
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := mgr.Exec(ctx, "agent-alpha", sessA.ID, "echo TETHER_PTY_OK", ".", 8*time.Second)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d (output: %q)", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "TETHER_PTY_OK") {
		t.Fatalf("expected output to contain TETHER_PTY_OK, got %q", res.Output)
	}

	lines, _, err := mgr.Screen("agent-alpha", sessA.ID, 20)
	if err != nil {
		t.Fatalf("Screen failed: %v", err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "TETHER_PTY_OK") {
		t.Fatalf("expected Screen lines to contain TETHER_PTY_OK, got %v", lines)
	}

	if err := mgr.CloseSession("agent-alpha", sessA.ID); err != nil {
		t.Fatalf("CloseSession failed: %v", err)
	}
	if len(mgr.ListSessions("agent-alpha")) != 0 {
		t.Fatalf("expected 0 sessions for agent-alpha after CloseSession")
	}
}
