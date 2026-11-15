package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tether/internal/auth"
	"tether/internal/bus"
	"tether/internal/env"
	"tether/internal/pty"
)

func TestTUIModel_ViewAndConsentResolution(t *testing.T) {
	eb := bus.NewEventBus()
	sysInfo := env.Detect()
	authMgr, _ := auth.NewManager(eb, 30*time.Minute)
	ptyMgr := pty.NewManager(sysInfo, eb)
	defer ptyMgr.CloseAll()

	mdl := NewModel("127.0.0.1:7432", sysInfo, eb, authMgr, ptyMgr)
	defer mdl.Close()

	// Resize window
	updated, _ := mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	mdl = updated.(*Model)

	// Enqueue a consent request in background
	resCh := make(chan bus.ConsentResult, 1)
	go func() {
		res, err := eb.RequestConsent(context.Background(), bus.ConsentRequest{
			ID:        "req-tui-1",
			Kind:      bus.ConsentKindConnect,
			AgentID:   "planner-01",
			AgentName: "Planner Agent",
			Workspace: "/repo/project",
			Reason:    "New agent connection",
		})
		if err == nil {
			resCh <- res
		}
	}()

	// Wait until queued
	deadline := time.Now().Add(time.Second)
	for len(eb.PendingConsents()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	view := mdl.View()
	if !strings.Contains(view, "127.0.0.1:7432") {
		t.Fatalf("expected header to contain server address, got:\n%s", view)
	}
	if !strings.Contains(view, "planner-01") {
		t.Fatalf("expected bottom consent panel to show pending request for planner-01, got:\n%s", view)
	}

	// Simulate operator pressing 'a' ([A]probar una vez)
	updated, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	mdl = updated.(*Model)

	select {
	case res := <-resCh:
		if res.Decision != bus.DecisionAllowOnce {
			t.Fatalf("expected DecisionAllowOnce from 'a' key, got %s", res.Decision)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for TUI 'a' key to resolve consent request")
	}
}
