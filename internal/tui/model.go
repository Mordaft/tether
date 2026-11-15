package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tether/internal/auth"
	"tether/internal/bus"
	"tether/internal/env"
	"tether/internal/pty"
)

// BusEventMsg wraps an internal bus.Event for the Bubbletea update loop.
type BusEventMsg struct {
	Event bus.Event
}

// TickMsg triggers periodic UI refreshes (e.g. token TTL / active agent counters).
type TickMsg time.Time

// Model implements the Bubbletea TUI for Tether.
type Model struct {
	listenAddr  string
	sysInfo     env.SystemInfo
	eventBus    *bus.EventBus
	authMgr     *auth.Manager
	ptyMgr      *pty.Manager
	eventsCh    <-chan bus.Event
	unsubscribe func()

	width        int
	height       int
	tabs         []bus.TerminalSession
	activeTabIdx int
	inputMode    bool // True when human operator toggled Ctrl+T to type directly into PTY

	viewport  viewport.Model
	textInput textinput.Model
	eventLog  []string
}

// NewModel initializes the multi-tab Bubbletea TUI model connected to the EventBus.
func NewModel(
	listenAddr string,
	sysInfo env.SystemInfo,
	eventBus *bus.EventBus,
	authMgr *auth.Manager,
	ptyMgr *pty.Manager,
) *Model {
	ch, unsub := eventBus.Subscribe(256)

	ti := textinput.New()
	ti.Placeholder = "Escribe comando/input para enviar a la PTY y pulsa Enter (Esc o Ctrl+T para salir)..."
	ti.CharLimit = 1024
	ti.Width = 80

	vp := viewport.New(100, 18)
	vp.SetContent("Sin sesiones PTY activas. Esperando conexiones de agentes vía MCP...")

	return &Model{
		listenAddr:  listenAddr,
		sysInfo:     sysInfo,
		eventBus:    eventBus,
		authMgr:     authMgr,
		ptyMgr:      ptyMgr,
		eventsCh:    ch,
		unsubscribe: unsub,
		viewport:    vp,
		textInput:   ti,
		eventLog:    []string{fmt.Sprintf("[%s] Servidor tether activo en %s", time.Now().Format("15:04:05"), listenAddr)},
	}
}

// Close releases the EventBus subscription.
func (m *Model) Close() {
	if m.unsubscribe != nil {
		m.unsubscribe()
	}
}

// Init starts listening to the EventBus channel and periodic refresh ticker.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		waitForBusEvent(m.eventsCh),
		tickEverySecond(),
	)
}

func waitForBusEvent(ch <-chan bus.Event) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return nil
		}
		return BusEventMsg{Event: evt}
	}
}

func tickEverySecond() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}

// Update processes keyboard shortcuts, terminal resize events, and real-time EventBus messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.recalculateLayout()
		m.refreshViewportContent()
		return m, nil

	case TickMsg:
		m.syncTabs()
		return m, tickEverySecond()

	case BusEventMsg:
		m.handleBusEvent(msg.Event)
		cmds = append(cmds, waitForBusEvent(m.eventsCh))
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		// If human operator is in Direct Terminal Input Mode (Ctrl+T)
		if m.inputMode {
			switch msg.String() {
			case "ctrl+t", "esc":
				m.inputMode = false
				m.textInput.Blur()
				return m, nil
			case "ctrl+c":
				if tab, ok := m.activeTab(); ok && m.ptyMgr != nil {
					_ = m.ptyMgr.WriteHumanInput(tab.AgentID, tab.ID, "Ctrl+C", false)
					m.appendLog(fmt.Sprintf("Señal Ctrl+C enviada por humano a %s", tab.Name))
				}
				return m, nil
			case "enter":
				val := m.textInput.Value()
				m.textInput.SetValue("")
				if tab, ok := m.activeTab(); ok && m.ptyMgr != nil {
					_ = m.ptyMgr.WriteHumanInput(tab.AgentID, tab.ID, val, true)
					m.appendLog(fmt.Sprintf("Input enviado a %s: %s", tab.Name, val))
				}
				return m, nil
			default:
				var tiCmd tea.Cmd
				m.textInput, tiCmd = m.textInput.Update(msg)
				return m, tiCmd
			}
		}

		// Normal TUI Supervision Mode shortcuts
		switch msg.String() {
		case "ctrl+c", "q", "Q":
			m.Close()
			return m, tea.Quit

		case "ctrl+t":
			if _, ok := m.activeTab(); ok {
				m.inputMode = true
				m.textInput.Focus()
				return m, textinput.Blink
			}
			return m, nil

		case "tab", "right", "l":
			if len(m.tabs) > 0 {
				m.activeTabIdx = (m.activeTabIdx + 1) % len(m.tabs)
				m.refreshViewportContent()
			}
			return m, nil

		case "shift+tab", "left", "h":
			if len(m.tabs) > 0 {
				m.activeTabIdx = (m.activeTabIdx - 1 + len(m.tabs)) % len(m.tabs)
				m.refreshViewportContent()
			}
			return m, nil

		case "ctrl+x":
			if tab, ok := m.activeTab(); ok && m.ptyMgr != nil {
				_ = m.ptyMgr.CloseSession(tab.AgentID, tab.ID)
				m.appendLog(fmt.Sprintf("Pestaña cerrada por humano: %s", tab.Name))
				m.syncTabs()
				m.refreshViewportContent()
			}
			return m, nil

		case "a", "A":
			if req, err := m.eventBus.ResolveNextConsent(bus.DecisionAllowOnce, "aprobado una vez desde TUI"); err == nil {
				m.appendLog(fmt.Sprintf("[APROBADO 1x] %s (%s)", req.AgentID, req.Kind))
			}
			return m, nil

		case "s", "S":
			if req, err := m.eventBus.ResolveNextConsent(bus.DecisionAllowSession, "aprobado para la sesión desde TUI"); err == nil {
				m.appendLog(fmt.Sprintf("[APROBADO SESIÓN] %s (%s)", req.AgentID, req.Kind))
			}
			return m, nil

		case "d", "D":
			if req, err := m.eventBus.ResolveNextConsent(bus.DecisionDeny, "denegado desde TUI"); err == nil {
				m.appendLog(fmt.Sprintf("[DENEGADO] %s (%s)", req.AgentID, req.Kind))
			}
			return m, nil
		}

		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		return m, vpCmd
	}

	return m, nil
}

func (m *Model) handleBusEvent(evt bus.Event) {
	ts := evt.Timestamp.Format("15:04:05")
	switch evt.Type {
	case bus.EventConsentQueued:
		if evt.ConsentRequest != nil {
			if evt.ConsentRequest.Kind == bus.ConsentKindConnect {
				m.appendLog(fmt.Sprintf("[%s] Solicitud conexión: %s -> %s", ts, evt.ConsentRequest.AgentID, evt.ConsentRequest.Workspace))
			} else {
				m.appendLog(fmt.Sprintf("[%s] Solicitud comando (%s): %s", ts, evt.ConsentRequest.AgentID, evt.ConsentRequest.Command))
			}
		}
	case bus.EventAgentConnected:
		if evt.AgentSession != nil {
			m.appendLog(fmt.Sprintf("[%s] Agente conectado: %s (%s)", ts, evt.AgentSession.AgentID, evt.AgentSession.WorkspaceRoot))
		}
	case bus.EventAgentExpired:
		if evt.AgentSession != nil {
			m.appendLog(fmt.Sprintf("[%s] Token expirado: %s", ts, evt.AgentSession.AgentID))
		}
	case bus.EventSessionOpened:
		m.syncTabs()
		if evt.TerminalSession != nil {
			m.selectTabByID(evt.TerminalSession.ID)
			m.appendLog(fmt.Sprintf("[%s] Nueva PTY: %s", ts, evt.TerminalSession.Name))
		}
		m.refreshViewportContent()
	case bus.EventSessionUpdated, bus.EventSessionOutput:
		m.syncTabs()
		m.refreshViewportContent()
	case bus.EventSessionClosed:
		m.syncTabs()
		m.refreshViewportContent()
	}
}

func (m *Model) syncTabs() {
	if m.ptyMgr == nil {
		return
	}
	all := m.ptyMgr.AllSessions()
	m.tabs = all
	if len(m.tabs) == 0 {
		m.activeTabIdx = 0
		m.inputMode = false
		return
	}
	if m.activeTabIdx >= len(m.tabs) {
		m.activeTabIdx = len(m.tabs) - 1
	}
}

func (m *Model) selectTabByID(id bus.SessionID) {
	for i, t := range m.tabs {
		if t.ID == id {
			m.activeTabIdx = i
			return
		}
	}
}

func (m *Model) activeTab() (bus.TerminalSession, bool) {
	if len(m.tabs) == 0 || m.activeTabIdx < 0 || m.activeTabIdx >= len(m.tabs) {
		return bus.TerminalSession{}, false
	}
	return m.tabs[m.activeTabIdx], true
}

func (m *Model) refreshViewportContent() {
	tab, ok := m.activeTab()
	if !ok || m.ptyMgr == nil {
		m.viewport.SetContent("Sin sesiones PTY activas.\nLos agentes conectados pueden abrir pestañas mediante la tool MCP `session_open`.")
		return
	}
	screen := m.ptyMgr.RawScreenForTUI(tab.AgentID, tab.ID)
	if strings.TrimSpace(screen) == "" {
		screen = fmt.Sprintf("(Sesión %s iniciada en %s — esperando comandos...)", tab.Name, tab.WorkspaceRoot)
	}
	m.viewport.SetContent(screen)
	m.viewport.GotoBottom()
}

func (m *Model) recalculateLayout() {
	w := m.width
	if w <= 0 {
		w = 100
	}
	h := m.height
	if h <= 0 {
		h = 30
	}
	vpHeight := h - 14
	if vpHeight < 6 {
		vpHeight = 6
	}
	m.viewport.Width = w - 4
	m.viewport.Height = vpHeight
	m.textInput.Width = w - 18
}

func (m *Model) appendLog(entry string) {
	m.eventLog = append(m.eventLog, entry)
	if len(m.eventLog) > 5 {
		m.eventLog = m.eventLog[len(m.eventLog)-5:]
	}
}

// View renders the 3-zone TUI interface:
// 1. Header (Server status, OS/Shells, Connected Agents)
// 2. Main Multi-Tab Terminal Area (Agent PTY tabs, live output viewport, Ctrl+T intervention bar)
// 3. Bottom Common Panel (FIFO Consent Queue + Quick Shortcuts + Event Log)
func (m *Model) View() string {
	return fmt.Sprintf(
		"%s\n%s\n%s",
		m.renderHeader(),
		m.renderMainArea(),
		m.renderConsentPanel(),
	)
}

func (m *Model) renderHeader() string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FAFAFA")).
		Background(lipgloss.Color("#1E293B")).
		Padding(0, 1)

	badgeOnline := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#10B981")).
		Render("● ONLINE")

	shellNames := make([]string, 0, len(m.sysInfo.Shells))
	for _, s := range m.sysInfo.Shells {
		shellNames = append(shellNames, s.Name)
	}
	shellsStr := strings.Join(shellNames, ", ")
	if shellsStr == "" {
		shellsStr = "none"
	}

	activeAgents := 0
	if m.authMgr != nil {
		activeAgents = len(m.authMgr.ActiveSessions())
	}

	line := fmt.Sprintf(
		"TETHER %s | %s | OS: %s/%s | Shells: [%s] | Agentes: %d | Pestañas PTY: %d",
		badgeOnline,
		m.listenAddr,
		m.sysInfo.OS,
		m.sysInfo.Arch,
		shellsStr,
		activeAgents,
		len(m.tabs),
	)
	return headerStyle.Render(line)
}

func (m *Model) renderMainArea() string {
	activeTabStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#0F172A")).
		Background(lipgloss.Color("#38BDF8")).
		Padding(0, 1)

	inactiveTabStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#94A3B8")).
		Background(lipgloss.Color("#1E293B")).
		Padding(0, 1)

	waitingTabStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#0F172A")).
		Background(lipgloss.Color("#F59E0B")).
		Padding(0, 1)

	var tabRendered []string
	for i, tab := range m.tabs {
		stateBadge := "●"
		switch tab.State {
		case bus.StateRunning:
			stateBadge = "⟳"
		case bus.StateWaitingForInput:
			stateBadge = "⚠ INPUT"
		case bus.StateClosed:
			stateBadge = "✕"
		}
		label := fmt.Sprintf("%s %s", stateBadge, tab.Name)
		if i == m.activeTabIdx {
			if tab.State == bus.StateWaitingForInput {
				tabRendered = append(tabRendered, waitingTabStyle.Render(label))
			} else {
				tabRendered = append(tabRendered, activeTabStyle.Render(label))
			}
		} else {
			tabRendered = append(tabRendered, inactiveTabStyle.Render(label))
		}
	}

	tabBar := " [Sin pestañas activas]"
	if len(tabRendered) > 0 {
		tabBar = strings.Join(tabRendered, " ")
	}

	var metaLine string
	if tab, ok := m.activeTab(); ok {
		metaStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#CBD5E1"))
		metaLine = metaStyle.Render(fmt.Sprintf(
			" Agente: %s | Workspace: %s | CWD: %s | Estado: %s | Cmd: %s",
			tab.AgentID,
			tab.WorkspaceRoot,
			tab.RelCWD,
			tab.State,
			 tab.ActiveCommand,
		))
	} else {
		metaLine = lipgloss.NewStyle().Foreground(lipgloss.Color("#64748B")).Render(" Esperando apertura de sesión PTY...")
	}

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#334155")).
		Padding(0, 1)

	inputBar := ""
	if m.inputMode {
		inputStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FDE047"))
		inputBar = "\n" + inputStyle.Render("INTERVENCIÓN HUMANA > ") + m.textInput.View()
	}

	return fmt.Sprintf("%s\n%s\n%s%s", tabBar, metaLine, boxStyle.Render(m.viewport.View()), inputBar)
}

func (m *Model) renderConsentPanel() string {
	pending := m.eventBus.PendingConsents()

	panelBorderColor := lipgloss.Color("#475569")
	if len(pending) > 0 {
		panelBorderColor = lipgloss.Color("#F59E0B")
	}

	panelStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(panelBorderColor).
		Padding(0, 1)

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F8FAFC"))
	warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FBBF24"))
	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#94A3B8"))

	var sb strings.Builder
	sb.WriteString(titleStyle.Render(fmt.Sprintf("CENTRO DE CONSENTIMIENTOS Y EVENTOS (Cola FIFO: %d pendientes)", len(pending))))
	sb.WriteString("\n")

	if len(pending) > 0 {
		head := pending[0]
		if head.Kind == bus.ConsentKindConnect {
			sb.WriteString(warnStyle.Render(fmt.Sprintf(
				"▶ [CONEXIÓN AGENTE] %s (%s) solicita acceso a Workspace: %s",
				head.AgentName,
				head.AgentID,
				head.Workspace,
			)))
		} else {
			sb.WriteString(warnStyle.Render(fmt.Sprintf(
				"▶ [EJECUCIÓN COMANDO] Agente: %s | Sesión: %s | CWD: %s | Comando: %s",
				head.AgentID,
				head.SessionID,
				head.RelCWD,
				head.Command,
			)))
		}
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("  Motivo: %s\n", head.Reason))
		sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#38BDF8")).Render(
			"  Acción: [A]probar una vez   [S]esión (Permitir en esta sesión)   [D]enegar",
		))
	} else {
		sb.WriteString(helpStyle.Render("✓ No hay solicitudes pendientes de aprobación humana."))
		if len(m.eventLog) > 0 {
			sb.WriteString("\n")
			sb.WriteString(helpStyle.Render("Último evento: " + m.eventLog[len(m.eventLog)-1]))
		}
	}

	sb.WriteString("\n")
	sb.WriteString(helpStyle.Render("Atajos: [Tab/Shift+Tab] Cambiar pestaña | [Ctrl+T] Escribir en PTY | [Ctrl+X] Cerrar pestaña | [A]probar [S]esión [D]enegar | [Q] Salir"))

	return panelStyle.Render(sb.String())
}
