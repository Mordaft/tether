package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"

	"tether/internal/bus"
)

// OverlayKind identifies which floating modal is currently active on the Z-axis.
type OverlayKind int

const (
	OverlayNone OverlayKind = iota
	OverlayConsent
	OverlayFuzzySwitcher
	OverlayInspectOutput
	OverlayWorkspaces
	OverlayKillConfirm
	OverlayWhichKey
)

var destructiveKeywords = []string{
	"rm ", "rm\t", "rmdir", "del ", "Remove-Item", "git reset", "git clean",
	"git push", "kill", "pkill", "dd ", "chmod", "chown", "format",
}

func isDestructiveCommand(cmd string) bool {
	lower := strings.ToLower(cmd)
	for _, kw := range destructiveKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// RenderActiveOverlay renders the active floating window (if any) and composites it
// over the dimmed background canvas.
func (m *Model) RenderActiveOverlay(background string) string {
	active := m.effectiveOverlay()
	if active == OverlayNone {
		return background
	}

	w := m.width
	if w <= 0 {
		w = 100
	}
	h := m.height
	if h <= 0 {
		h = 30
	}

	switch active {
	case OverlayWhichKey:
		box := m.renderWhichKeyModal()
		return placeOverlayBottomRight(background, box, w, h)

	case OverlayConsent:
		box := m.renderConsentModal(w)
		return lipgloss.Place(w, h-2, lipgloss.Center, lipgloss.Center, box,
			lipgloss.WithWhitespaceBackground(BgBase),
		)

	case OverlayFuzzySwitcher:
		box := m.renderFuzzySwitcherModal(w)
		return lipgloss.Place(w, h-2, lipgloss.Center, lipgloss.Center, box,
			lipgloss.WithWhitespaceBackground(BgBase),
		)

	case OverlayInspectOutput:
		box := m.renderInspectModal(w, h)
		return lipgloss.Place(w, h-2, lipgloss.Center, lipgloss.Center, box,
			lipgloss.WithWhitespaceBackground(BgBase),
		)

	case OverlayWorkspaces:
		box := m.renderWorkspacesModal(w)
		return lipgloss.Place(w, h-2, lipgloss.Center, lipgloss.Center, box,
			lipgloss.WithWhitespaceBackground(BgBase),
		)

	case OverlayKillConfirm:
		box := m.renderKillConfirmModal(w)
		return lipgloss.Place(w, h-2, lipgloss.Center, lipgloss.Center, box,
			lipgloss.WithWhitespaceBackground(BgBase),
		)

	default:
		return background
	}
}

// effectiveOverlay determines the highest-priority floating window to display.
func (m *Model) effectiveOverlay() OverlayKind {
	if m.leaderWhichKeyVisible {
		return OverlayWhichKey
	}
	if m.overlay != OverlayNone {
		return m.overlay
	}
	if len(m.eventBus.PendingConsents()) > 0 && !m.consentDismissed {
		return OverlayConsent
	}
	return OverlayNone
}

// 1. Consent Modal (60% width centered, AccentWarn or AccentDanger border + Chroma syntax highlighting)
func (m *Model) renderConsentModal(totalWidth int) string {
	pending := m.eventBus.PendingConsents()
	if len(pending) == 0 {
		return ""
	}
	if m.consentCursor >= len(pending) {
		m.consentCursor = 0
	}
	req := pending[m.consentCursor]

	modalWidth := (totalWidth * 60) / 100
	if modalWidth < 54 {
		modalWidth = 54
	}
	if modalWidth > totalWidth-4 {
		modalWidth = totalWidth - 4
	}

	destructive := req.Kind == bus.ConsentKindCommand && isDestructiveCommand(req.Command)
	boxStyle := m.styles.FloatWarn.Width(modalWidth)
	headerBadge := lipgloss.NewStyle().Bold(true).Foreground(BgBase).Background(AccentWarn).Padding(0, 1).
		Render(fmt.Sprintf("%s SOLICITUD DE CONSENTIMIENTO (%d/%d)", m.icons.StatePerm, m.consentCursor+1, len(pending)))

	if destructive {
		boxStyle = m.styles.FloatDanger.Width(modalWidth)
		headerBadge = lipgloss.NewStyle().Bold(true).Foreground(BgBase).Background(AccentDanger).Padding(0, 1).
			Render(fmt.Sprintf("%s COMANDO SENSIBLE / DESTRUCTIVO (%d/%d)", m.icons.StateWait, m.consentCursor+1, len(pending)))
	}

	agentLabel := lipgloss.NewStyle().Bold(true).Foreground(Secondary).Render(fmt.Sprintf("%s %s (%s)", m.icons.Agent, req.AgentName, req.AgentID))
	wsLabel := m.styles.MetadataItalic.Render(fmt.Sprintf("%s Workspace: %s", m.icons.Dir, req.Workspace))

	var body strings.Builder
	body.WriteString(headerBadge + "\n\n")
	body.WriteString("Agente:    " + agentLabel + "\n")
	body.WriteString("Contexto:  " + wsLabel + "\n")

	if req.Kind == bus.ConsentKindConnect {
		body.WriteString("\n")
		body.WriteString(lipgloss.NewStyle().Bold(true).Foreground(Primary).Render("Handshake POST /auth/connect") + "\n")
		body.WriteString(fmt.Sprintf("Motivo: %s\n", req.Reason))
	} else {
		cwdLabel := m.styles.MetadataItalic.Render(req.RelCWD)
		body.WriteString(fmt.Sprintf("CWD Rel.:  %s (Sesión: %s)\n\n", cwdLabel, req.SessionID))
		body.WriteString(lipgloss.NewStyle().Faint(true).Foreground(FgMuted).Render("Comando solicitado:") + "\n")
		body.WriteString("  " + HighlightCommand(req.Command, "bash") + "\n\n")
		body.WriteString(m.styles.MetadataItalic.Render("Política: "+req.Reason) + "\n")
	}

	body.WriteString("\n")
	btnApprove := lipgloss.NewStyle().Bold(true).Foreground(BgBase).Background(AccentNormal).Padding(0, 1).Render("[a] Aprobar 1x")
	btnSession := lipgloss.NewStyle().Bold(true).Foreground(BgBase).Background(Primary).Padding(0, 1).Render("[s] Aprobar en sesión")
	btnDeny := lipgloss.NewStyle().Bold(true).Foreground(BgBase).Background(AccentDanger).Padding(0, 1).Render("[d] Denegar")
	btnTab := lipgloss.NewStyle().Foreground(FgMuted).Render("[Tab] Siguiente  [Esc] Ocultar")

	body.WriteString(fmt.Sprintf("%s  %s  %s  %s", btnApprove, btnSession, btnDeny, btnTab))

	return boxStyle.Render(body.String())
}

// 2. Fuzzy Session Switcher (70% width centered, sahilm/fuzzy live filtering)
func (m *Model) renderFuzzySwitcherModal(totalWidth int) string {
	modalWidth := (totalWidth * 70) / 100
	if modalWidth < 56 {
		modalWidth = 56
	}
	if modalWidth > totalWidth-4 {
		modalWidth = totalWidth - 4
	}

	boxStyle := m.styles.FloatWindow.Width(modalWidth)
	title := m.styles.ModalTitle.Render(fmt.Sprintf("%s TELESCOPE SESSION SWITCHER (Fuzzy Finder)", m.icons.ShellBash))

	var sb strings.Builder
	sb.WriteString(title + "\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(Primary).Bold(true).Render("Prompt > ") + m.fuzzyInput.View() + "\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(BorderMuted).Render(strings.Repeat("─", modalWidth-6)) + "\n")

	filtered := m.filteredFuzzyTabs()
	if len(filtered) == 0 {
		sb.WriteString(m.styles.MetadataItalic.Render("  (Sin sesiones coincidentes)") + "\n")
	} else {
		if m.fuzzyCursor >= len(filtered) {
			m.fuzzyCursor = 0
		}
		for i, item := range filtered {
			if i >= 10 {
				break
			}
			tab := item.tab
			line := fmt.Sprintf(" %s %-24s │ %s %-14s │ %s %-8s │ %s %s",
				m.icons.ForState(tab.State),
				tab.Name,
				m.icons.Agent,
				tab.AgentID,
				m.icons.ForShell(tab.Shell),
				tab.Shell,
				m.icons.Dir,
				tab.RelCWD,
			)
			if i == m.fuzzyCursor {
				selStyle := lipgloss.NewStyle().Bold(true).Foreground(FgPrimary).Background(BgSurface)
				sb.WriteString(selStyle.Render("▶"+line) + "\n")
			} else {
				sb.WriteString(lipgloss.NewStyle().Foreground(FgMuted).Render(" "+line) + "\n")
			}
		}
	}

	sb.WriteString("\n" + m.styles.MetadataItalic.Render("[↑/↓ o Ctrl+n/p] Seleccionar   [Enter] Saltar a pestaña   [Esc] Cerrar"))
	return boxStyle.Render(sb.String())
}

type fuzzyTabMatch struct {
	index int
	tab   bus.TerminalSession
}

func (m *Model) filteredFuzzyTabs() []fuzzyTabMatch {
	query := strings.TrimSpace(m.fuzzyInput.Value())
	if query == "" {
		out := make([]fuzzyTabMatch, len(m.tabs))
		for i, t := range m.tabs {
			out[i] = fuzzyTabMatch{index: i, tab: t}
		}
		return out
	}

	candidates := make([]string, len(m.tabs))
	for i, t := range m.tabs {
		candidates[i] = fmt.Sprintf("%s %s %s %s %s", t.Name, t.AgentID, t.Shell, t.State, t.RelCWD)
	}

	matches := fuzzy.Find(query, candidates)
	out := make([]fuzzyTabMatch, 0, len(matches))
	for _, match := range matches {
		out = append(out, fuzzyTabMatch{
			index: match.Index,
			tab:   m.tabs[match.Index],
		})
	}
	return out
}

// 3. Inspect Output Viewer (Space + o: 85% width/height with Chroma syntax highlighting)
func (m *Model) renderInspectModal(totalWidth, totalHeight int) string {
	modalWidth := (totalWidth * 85) / 100
	if modalWidth < 60 {
		modalWidth = 60
	}
	modalHeight := (totalHeight * 80) / 100
	if modalHeight < 12 {
		modalHeight = 12
	}

	boxStyle := m.styles.FloatWindow.Width(modalWidth)
	title := m.styles.ModalTitle.Render("INSPECT OUTPUT VIEWER (Chroma Syntax Highlighted)")

	lexer := DetectOutputLexer(strings.Join(m.inspectLines, "\n"))
	viewRows := modalHeight - 6
	if viewRows < 4 {
		viewRows = 4
	}

	start := m.inspectScroll
	if start > len(m.inspectLines) {
		start = len(m.inspectLines)
	}
	end := start + viewRows
	if end > len(m.inspectLines) {
		end = len(m.inspectLines)
	}

	slice := m.inspectLines[start:end]
	highlighted := HighlightCommand(strings.Join(slice, "\n"), lexer)
	if strings.TrimSpace(highlighted) == "" {
		highlighted = m.styles.MetadataItalic.Render("(Buffer de salida vacío)")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s  %s\n", title, m.styles.MetadataItalic.Render(fmt.Sprintf("[lexer: %s | líneas %d-%d de %d]", lexer, start+1, end, len(m.inspectLines)))))
	sb.WriteString(lipgloss.NewStyle().Foreground(BorderMuted).Render(strings.Repeat("─", modalWidth-6)) + "\n")
	sb.WriteString(highlighted + "\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(BorderMuted).Render(strings.Repeat("─", modalWidth-6)) + "\n")
	sb.WriteString(m.styles.MetadataItalic.Render("[j/k/Ctrl+d/u/g/G] Scroll   [y] Yank al portapapeles   [Esc/q] Cerrar"))

	return boxStyle.Render(sb.String())
}

// 4. Workspaces & Agents Modal (Space + w)
func (m *Model) renderWorkspacesModal(totalWidth int) string {
	modalWidth := (totalWidth * 65) / 100
	if modalWidth < 56 {
		modalWidth = 56
	}

	boxStyle := m.styles.FloatWindow.Width(modalWidth)
	title := m.styles.ModalTitle.Render(fmt.Sprintf("%s WORKSPACES & CONNECTED AGENTS", m.icons.Agent))

	var sb strings.Builder
	sb.WriteString(title + "\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(BorderMuted).Render(strings.Repeat("─", modalWidth-6)) + "\n")

	var sessions []*bus.AgentSession
	if m.authMgr != nil {
		sessions = m.authMgr.ActiveSessions()
	}

	if len(sessions) == 0 {
		sb.WriteString(m.styles.MetadataItalic.Render("No hay sesiones de agentes autenticadas activas.") + "\n")
	} else {
		for _, s := range sessions {
			rem := formatRemainingTTL(time.Until(s.ExpiresAt))
			agentLine := fmt.Sprintf("%s %-20s │ %s TTL: %-7s │ %s %s",
				m.icons.Agent,
				s.AgentID,
				m.icons.TTL,
				rem,
				m.icons.Dir,
				s.WorkspaceRoot,
			)
			sb.WriteString(lipgloss.NewStyle().Foreground(FgPrimary).Render(agentLine) + "\n")
		}
	}

	sb.WriteString("\n" + m.styles.MetadataItalic.Render("[Esc / q / Enter] Cerrar ventana"))
	return boxStyle.Render(sb.String())
}

// 5. Kill Process Group Confirmation Modal (Space + k)
func (m *Model) renderKillConfirmModal(totalWidth int) string {
	modalWidth := (totalWidth * 55) / 100
	if modalWidth < 50 {
		modalWidth = 50
	}
	boxStyle := m.styles.FloatDanger.Width(modalWidth)

	tabName := "ninguna"
	if tab, ok := m.activeTab(); ok {
		tabName = fmt.Sprintf("%s (Agente: %s)", tab.Name, tab.AgentID)
	}

	var sb strings.Builder
	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(AccentDanger).Render(fmt.Sprintf("%s KILL PROCESS GROUP", m.icons.StateWait)) + "\n\n")
	sb.WriteString(fmt.Sprintf("¿Confirmas terminar el árbol completo de subprocesos de %s?\n\n", lipgloss.NewStyle().Bold(true).Render(tabName)))
	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(BgBase).Background(AccentDanger).Padding(0, 1).Render("[y / Enter] Matar árbol PTY") + "   ")
	sb.WriteString(lipgloss.NewStyle().Foreground(FgMuted).Render("[n / Esc] Cancelar"))

	return boxStyle.Render(sb.String())
}

// 6. Which-Key Compact Floating Window (Bottom-Right corner after 300ms Space leader hold)
func (m *Model) renderWhichKeyModal() string {
	boxStyle := m.styles.FloatWindow.Padding(0, 2)
	title := m.styles.ModalTitle.Render("Leader [Space] — which-key")

	var sb strings.Builder
	sb.WriteString(title + "\n")
	for _, entry := range LeaderMenuEntries {
		k := lipgloss.NewStyle().Bold(true).Foreground(Primary).Render(fmt.Sprintf("%-9s", entry.Key))
		arrow := lipgloss.NewStyle().Foreground(FgMuted).Render("➜ ")
		desc := lipgloss.NewStyle().Foreground(FgPrimary).Render(entry.Desc)
		sb.WriteString(fmt.Sprintf(" %s %s%s\n", k, arrow, desc))
	}
	return boxStyle.Render(strings.TrimRight(sb.String(), "\n"))
}

// placeOverlayBottomRight composites the Which-Key popup in the bottom-right corner
// above the statusline while keeping the dimmed terminal viewport visible behind it.
func placeOverlayBottomRight(background, modal string, width, height int) string {
	bgLines := strings.Split(background, "\n")
	modalLines := strings.Split(modal, "\n")

	if len(bgLines) == 0 {
		return modal
	}

	startRow := len(bgLines) - len(modalLines) - 1
	if startRow < 0 {
		startRow = 0
	}

	for i, mLine := range modalLines {
		targetRow := startRow + i
		if targetRow >= len(bgLines) {
			break
		}
		mWidth := lipgloss.Width(mLine)
		padLeft := width - mWidth - 2
		if padLeft < 0 {
			padLeft = 0
		}
		bgLines[targetRow] = strings.Repeat(" ", padLeft) + mLine
	}

	return strings.Join(bgLines, "\n")
}
