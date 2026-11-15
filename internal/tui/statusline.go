package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"tether/internal/bus"
)

// RenderTopTabBar renders the 1-line top tab bar showing active/inactive PTY sessions
// with state icons on the left and the amber pending approvals badge on the right.
func (m *Model) RenderTopTabBar() string {
	w := m.width
	if w <= 0 {
		w = 100
	}

	var leftParts []string
	if len(m.tabs) == 0 {
		emptyTab := m.styles.TabInactive.Render(fmt.Sprintf("%s tether (%s)", m.icons.ShellBash, m.listenAddr))
		leftParts = append(leftParts, emptyTab)
	} else {
		for i, tab := range m.tabs {
			stIcon := m.icons.ForState(tab.State)
			label := fmt.Sprintf("%s %s", stIcon, tab.Name)
			switch {
			case i == m.activeTabIdx && tab.State == bus.StateWaitingForInput:
				leftParts = append(leftParts, m.styles.TabWaiting.Render(label))
			case i == m.activeTabIdx:
				leftParts = append(leftParts, m.styles.TabActive.Render(label))
			case tab.State == bus.StateWaitingForInput:
				leftParts = append(leftParts, m.styles.TabWaiting.Render(label))
			default:
				leftParts = append(leftParts, m.styles.TabInactive.Render(label))
			}
		}
	}

	left := strings.Join(leftParts, " ")

	pendingCount := len(m.eventBus.PendingConsents())
	var right string
	if pendingCount > 0 {
		right = m.styles.BadgePending.Render(fmt.Sprintf("%s %d pending", m.icons.StatePerm, pendingCount))
	} else {
		right = lipgloss.NewStyle().
			Foreground(FgMuted).
			Background(BgSurface).
			Padding(0, 1).
			Render(m.listenAddr)
	}

	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)
	gap := w - leftWidth - rightWidth
	if gap < 1 {
		gap = 1
	}

	spacer := lipgloss.NewStyle().Background(BgBase).Render(strings.Repeat(" ", gap))
	return left + spacer + right
}

// RenderBottomStatusline renders the 1-line lualine-inspired bottom status bar:
// `[ NORMAL | TERMINAL ] │ 󰚩 <agent_id> │  <shell> │  <cwd_relativo> │ ⏱ <ttl_restante> │ [Space] Leader`
func (m *Model) RenderBottomStatusline() string {
	w := m.width
	if w <= 0 {
		w = 100
	}

	// If in active `/` search prompt mode, show the search bar on the statusline
	if m.searchInputActive {
		prompt := lipgloss.NewStyle().Bold(true).Foreground(Primary).Background(BgSurface).Padding(0, 1).Render("/")
		query := lipgloss.NewStyle().Foreground(FgPrimary).Background(BgBase).Render(m.searchInput.View())
		hint := m.styles.MetadataItalic.Render("  [Enter] buscar  [Esc] cancelar")
		return prompt + query + hint
	}

	var modeBadge string
	if m.mode == ModeTerminal {
		modeBadge = m.styles.ModeTerminal.Render("[ TERMINAL ]")
	} else {
		modeBadge = m.styles.ModeNormal.Render("[ NORMAL ]")
	}

	agentID := "no-agent"
	shellName := m.sysInfo.DefaultShell
	relCWD := "."
	ttlStr := "--:--"

	if tab, ok := m.activeTab(); ok {
		agentID = string(tab.AgentID)
		shellName = tab.Shell
		relCWD = tab.RelCWD
		ttlStr = m.lookupAgentTTL(tab.AgentID)
	} else if m.authMgr != nil {
		sessions := m.authMgr.ActiveSessions()
		if len(sessions) > 0 {
			agentID = string(sessions[0].AgentID)
			ttlStr = formatRemainingTTL(time.Until(sessions[0].ExpiresAt))
		}
	}

	sep := lipgloss.NewStyle().Foreground(BorderMuted).Background(BgSurface).Render("│")

	segAgent := m.styles.StatusAgent.Render(fmt.Sprintf("%s %s", m.icons.Agent, agentID))
	segShell := m.styles.StatusSegment.Render(fmt.Sprintf("%s %s", m.icons.ForShell(shellName), shellName))
	segCWD := m.styles.StatusSegment.Render(fmt.Sprintf("%s %s", m.icons.Dir, m.styles.MetadataItalic.Render(relCWD)))
	segTTL := m.styles.StatusSegment.Render(fmt.Sprintf("%s %s", m.icons.TTL, ttlStr))

	left := strings.Join([]string{modeBadge, segAgent, segShell, segCWD, segTTL}, sep)

	var right string
	if m.toastMessage != "" {
		right = m.styles.ToastSuccess.Render(m.toastMessage)
	} else if m.mode == ModeTerminal {
		right = m.styles.StatusLeader.Render("[Esc Esc] Normal")
	} else {
		right = m.styles.StatusLeader.Render("[Space] Leader")
	}

	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)
	gap := w - leftWidth - rightWidth
	if gap < 1 {
		gap = 1
	}

	fill := lipgloss.NewStyle().Background(BgSurface).Render(strings.Repeat(" ", gap))
	return left + fill + right
}

func (m *Model) lookupAgentTTL(agentID bus.AgentID) string {
	if m.authMgr == nil {
		return "--:--"
	}
	for _, s := range m.authMgr.ActiveSessions() {
		if s.AgentID == agentID {
			return formatRemainingTTL(time.Until(s.ExpiresAt))
		}
	}
	return "expired"
}

func formatRemainingTTL(d time.Duration) string {
	if d <= 0 {
		return "00:00"
	}
	mins := int(d.Minutes())
	secs := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", mins, secs)
}
