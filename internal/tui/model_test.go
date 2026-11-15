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

func TestThemeAndIconsFallback(t *testing.T) {
	nf := NewIconSet(false)
	if nf.Agent == "[AG]" {
		t.Fatalf("expected Nerd Font glyph when noIcons=false")
	}

	ascii := NewIconSet(true)
	if ascii.Agent != "[AG]" || ascii.Dir != "[DIR]" || ascii.StatePerm != "[PERM]" {
		t.Fatalf("expected ASCII fallback labels when noIcons=true, got %+v", ascii)
	}
	if ascii.ForShell("pwsh") != "[PS]" || ascii.ForShell("bash") != "[SH]" || ascii.ForShell("cmd.exe") != "[CMD]" {
		t.Fatalf("unexpected shell icon mapping in ASCII mode")
	}

	highlighted := HighlightCommand("ls -la src", "bash")
	if !strings.Contains(highlighted, "ls") {
		t.Fatalf("expected Chroma highlighted command to contain 'ls', got %q", highlighted)
	}
}

func TestVirtualViewport_ScrollSearchAndVirtualization(t *testing.T) {
	vp := NewVirtualViewport(80, 5)
	lines := make([]string, 100)
	for i := 0; i < 100; i++ {
		lines[i] = "log line entry"
	}
	lines[42] = "ERROR: critical database connection failure"
	vp.SetLines(lines)

	// Initially at bottom with AutoScroll = true
	if !vp.AutoScroll || vp.TopLine != 95 {
		t.Fatalf("expected AutoScroll=true at TopLine=95, got AutoScroll=%v TopLine=%d", vp.AutoScroll, vp.TopLine)
	}

	// ScrollUp freezes AutoScroll
	vp.ScrollUp(10)
	if vp.AutoScroll || vp.TopLine != 85 {
		t.Fatalf("expected AutoScroll=false at TopLine=85 after ScrollUp(10), got AutoScroll=%v TopLine=%d", vp.AutoScroll, vp.TopLine)
	}

	// Search jumps to line 42 and freezes AutoScroll
	vp.SetSearchQuery("critical")
	if len(vp.MatchLines) != 1 || vp.MatchLines[0] != 42 {
		t.Fatalf("expected match at line 42, got %v", vp.MatchLines)
	}

	// GotoBottom re-enables AutoScroll
	vp.GotoBottom()
	if !vp.AutoScroll || vp.TopLine != 95 {
		t.Fatalf("expected GotoBottom to restore AutoScroll=true and TopLine=95")
	}

	// Virtualized render outputs exactly Height (5) rows
	rendered := vp.RenderVirtualized(NewStyles(), false)
	rows := strings.Split(rendered, "\n")
	if len(rows) != 5 {
		t.Fatalf("expected exactly 5 virtualized rows, got %d", len(rows))
	}
}

func TestTUIModel_ModalNavigationLeaderAndConsentOverlay(t *testing.T) {
	eb := bus.NewEventBus()
	sysInfo := env.Detect()
	authMgr, _ := auth.NewManager(eb, 30*time.Minute)
	ptyMgr := pty.NewManager(sysInfo, eb)
	defer ptyMgr.CloseAll()

	mdl := NewModel("127.0.0.1:7432", sysInfo, eb, authMgr, ptyMgr, WithNoIcons(true))
	defer mdl.Close()

	updated, _ := mdl.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	mdl = updated.(*Model)

	// 1. Verify statusline shows [ NORMAL ] and [Space] Leader
	view := mdl.View()
	if !strings.Contains(view, "[ NORMAL ]") || !strings.Contains(view, "[Space] Leader") {
		t.Fatalf("expected lualine statusbar with [ NORMAL ] and [Space] Leader, got:\n%s", view)
	}

	// 2. Test Space Leader + 300ms Which-Key popup
	updated, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	mdl = updated.(*Model)
	if !mdl.leaderPending {
		t.Fatalf("expected leaderPending=true after pressing Space")
	}
	updated, _ = mdl.Update(WhichKeyTimeoutMsg{Seq: mdl.leaderSeq})
	mdl = updated.(*Model)
	if !mdl.leaderWhichKeyVisible {
		t.Fatalf("expected Which-Key popup visible after 300ms timeout")
	}
	wkView := mdl.View()
	if !strings.Contains(wkView, "which-key") || !strings.Contains(wkView, "Session Switcher") {
		t.Fatalf("expected Which-Key floating window in view, got:\n%s", wkView)
	}

	// 3. Press 'z' after Space to toggle Zen Mode
	updated, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	mdl = updated.(*Model)
	if !mdl.zenMode {
		t.Fatalf("expected Space + z to enable Zen Mode")
	}
	// Toggle Zen Mode back off with Space + z
	updated, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	mdl = updated.(*Model)
	updated, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	mdl = updated.(*Model)
	if mdl.zenMode {
		t.Fatalf("expected second Space + z to disable Zen Mode")
	}

	// 4. Enqueue a consent request and verify Floating Consent Modal + [a] approval
	resCh := make(chan bus.ConsentResult, 1)
	go func() {
		res, err := eb.RequestConsent(context.Background(), bus.ConsentRequest{
			ID:        "req-nvim-1",
			Kind:      bus.ConsentKindCommand,
			AgentID:   "coder-01",
			AgentName: "Coder",
			SessionID: "sess-1",
			Workspace: "/repo/app",
			RelCWD:    "src",
			Command:   "rm -rf dist",
			Reason:    "destructive command requires confirmation",
		})
		if err == nil {
			resCh <- res
		}
	}()

	deadline := time.Now().Add(time.Second)
	for len(eb.PendingConsents()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	mdl.dirty.Store(true)
	modalView := mdl.View()
	if !strings.Contains(modalView, "coder-01") || !strings.Contains(modalView, "1 pending") {
		t.Fatalf("expected floating consent modal and '1 pending' badge in view, got:\n%s", modalView)
	}

	// Approve with 'a'
	updated, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	mdl = updated.(*Model)

	select {
	case res := <-resCh:
		if res.Decision != bus.DecisionAllowOnce {
			t.Fatalf("expected DecisionAllowOnce, got %s", res.Decision)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for 'a' key to approve consent")
	}

	// 5. Open a real PTY tab and test ModeTerminal entry ('i') and Double-Esc ('Esc Esc') exit
	ws := t.TempDir()
	requestedShell := ""
	for _, s := range sysInfo.Shells {
		if s.Name == "cmd.exe" || s.Name == "sh" || s.Name == "bash" {
			requestedShell = s.Name
			break
		}
	}
	_, err := ptyMgr.OpenSession("coder-01", "Coder", ws, requestedShell, "main")
	if err != nil {
		t.Fatalf("OpenSession failed: %v", err)
	}
	mdl.syncTabs()

	// Press 'i' to enter TERMINAL mode
	updated, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	mdl = updated.(*Model)
	if mdl.mode != ModeTerminal {
		t.Fatalf("expected ModeTerminal after pressing 'i'")
	}
	if !strings.Contains(mdl.View(), "[ TERMINAL ]") {
		t.Fatalf("expected statusline to display [ TERMINAL ]")
	}

	// Press Esc Esc quickly to exit back to NORMAL mode
	updated, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mdl = updated.(*Model)
	updated, _ = mdl.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mdl = updated.(*Model)
	if mdl.mode != ModeNormal {
		t.Fatalf("expected double Esc (Esc Esc) to return to ModeNormal")
	}
}
