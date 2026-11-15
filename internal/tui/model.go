package tui

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tether/internal/auth"
	"tether/internal/bus"
	"tether/internal/env"
	"tether/internal/pty"
)

const FrameInterval60FPS = 16 * time.Millisecond

// Mode represents the Vim-like modal state of the TUI.
type Mode int

const (
	ModeNormal Mode = iota
	ModeTerminal
)

// BusEventMsg wraps an internal bus.Event for the Bubbletea update loop.
type BusEventMsg struct {
	Event bus.Event
}

// FrameTickMsg drives the decoupled 60 FPS (~16ms) render loop.
type FrameTickMsg time.Time

// ModelOption configures optional settings on the TUI Model.
type ModelOption func(*Model)

// WithNoIcons enables ASCII icon fallback (--no-icons) for terminals without Nerd Fonts v3.
func WithNoIcons(noIcons bool) ModelOption {
	return func(m *Model) {
		m.icons = NewIconSet(noIcons)
	}
}

// Model implements the Neovim/LazyVim/Zellij-inspired Bubbletea TUI for Tether.
type Model struct {
	listenAddr  string
	sysInfo     env.SystemInfo
	eventBus    *bus.EventBus
	authMgr     *auth.Manager
	ptyMgr      *pty.Manager
	eventsCh    <-chan bus.Event
	unsubscribe func()

	icons  IconSet
	styles Styles

	width        int
	height       int
	mode         Mode
	zenMode      bool
	tabs         []bus.TerminalSession
	activeTabIdx int

	// Virtualized viewport & in-buffer search
	viewport          VirtualViewport
	searchInput       textinput.Model
	searchInputActive bool

	// Leader key (Space) & 300ms Which-Key state
	leaderPending         bool
	leaderWhichKeyVisible bool
	leaderSeq             uint64

	// Double-Esc (`Esc Esc`) tracker for exiting ModeTerminal
	lastEscAt time.Time

	// Floating Window Overlays (Z-Index)
	overlay          OverlayKind
	consentCursor    int
	consentDismissed bool
	fuzzyInput       textinput.Model
	fuzzyCursor      int
	inspectLines     []string
	inspectScroll    int

	// Toast notifications
	toastMessage string
	toastSeq     uint64

	// 60 FPS decoupled dirty flag & cached frame
	dirty      atomic.Bool
	cachedView string
	lastSecond int64
}

// NewModel initializes the multi-tab Neovim-grade Bubbletea TUI connected to the EventBus.
func NewModel(
	listenAddr string,
	sysInfo env.SystemInfo,
	eventBus *bus.EventBus,
	authMgr *auth.Manager,
	ptyMgr *pty.Manager,
	opts ...ModelOption,
) *Model {
	ch, unsub := eventBus.Subscribe(256)

	searchTI := textinput.New()
	searchTI.Placeholder = "buscar en buffer..."
	searchTI.CharLimit = 256
	searchTI.Width = 40

	fuzzyTI := textinput.New()
	fuzzyTI.Placeholder = "filtrar por agent_id, sesión, shell o estado..."
	fuzzyTI.CharLimit = 256
	fuzzyTI.Width = 48

	vp := NewVirtualViewport(100, 24)
	vp.SetLines([]string{
		"Tether Side-Car Terminal Bridge — Listo.",
		"Esperando conexiones de agentes vía MCP (POST /auth/connect)...",
	})

	m := &Model{
		listenAddr:  listenAddr,
		sysInfo:     sysInfo,
		eventBus:    eventBus,
		authMgr:     authMgr,
		ptyMgr:      ptyMgr,
		eventsCh:    ch,
		unsubscribe: unsub,
		icons:       NewIconSet(false),
		styles:      NewStyles(),
		width:       100,
		height:      30,
		mode:        ModeNormal,
		viewport:    vp,
		searchInput: searchTI,
		fuzzyInput:  fuzzyTI,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}

	m.dirty.Store(true)
	return m
}

// Close releases the EventBus subscription.
func (m *Model) Close() {
	if m.unsubscribe != nil {
		m.unsubscribe()
	}
}

// Init starts the EventBus listener and the 60 FPS (~16ms) tick loop.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		waitForBusEvent(m.eventsCh),
		tick60FPS(),
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

func tick60FPS() tea.Cmd {
	return tea.Tick(FrameInterval60FPS, func(t time.Time) tea.Msg {
		return FrameTickMsg(t)
	})
}

// Update drives the modal state machine, floating overlays, Leader key sequences, and 60 FPS refresh.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.recalculateLayout()
		m.syncActiveBufferLines()
		m.dirty.Store(true)
		return m, nil

	case FrameTickMsg:
		nowSec := time.Time(msg).Unix()
		secChanged := nowSec != m.lastSecond
		if secChanged {
			m.lastSecond = nowSec
		}
		ptyDirty := m.ptyMgr != nil && m.ptyMgr.ConsumeDirty()
		if ptyDirty || secChanged {
			m.syncTabs()
			m.syncActiveBufferLines()
			m.dirty.Store(true)
		}
		return m, tick60FPS()

	case WhichKeyTimeoutMsg:
		if m.leaderPending && msg.Seq == m.leaderSeq {
			m.leaderWhichKeyVisible = true
			m.dirty.Store(true)
		}
		return m, nil

	case ToastExpireMsg:
		if msg.Seq == m.toastSeq {
			m.toastMessage = ""
			m.dirty.Store(true)
		}
		return m, nil

	case BusEventMsg:
		m.handleBusEvent(msg.Event)
		m.dirty.Store(true)
		return m, waitForBusEvent(m.eventsCh)

	case tea.KeyMsg:
		m.dirty.Store(true)
		return m.handleKeyMsg(msg)
	}

	return m, nil
}

func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// 1. TERMINAL / INSERT Mode: Forward raw bytes directly to PTY; double Esc (`Esc Esc`) returns to NORMAL.
	if m.mode == ModeTerminal {
		if msg.Type == tea.KeyEsc {
			now := time.Now()
			if !m.lastEscAt.IsZero() && now.Sub(m.lastEscAt) <= DoubleEscThreshold {
				m.mode = ModeNormal
				m.lastEscAt = time.Time{}
				cmd := m.showToast("Modo NORMAL")
				return m, cmd
			}
			m.lastEscAt = now
			if tab, ok := m.activeTab(); ok && m.ptyMgr != nil {
				_ = m.ptyMgr.WriteRaw(tab.AgentID, tab.ID, []byte{0x1b})
			}
			return m, nil
		}

		m.lastEscAt = time.Time{}
		if tab, ok := m.activeTab(); ok && m.ptyMgr != nil {
			if raw := KeyMsgToRawBytes(msg); len(raw) > 0 {
				_ = m.ptyMgr.WriteRaw(tab.AgentID, tab.ID, raw)
			}
		}
		return m, nil
	}

	// 2. Search Input Prompt (`/`) in NORMAL mode
	if m.searchInputActive {
		switch msg.Type {
		case tea.KeyEsc:
			m.searchInputActive = false
			m.searchInput.Blur()
			return m, nil
		case tea.KeyEnter:
			m.searchInputActive = false
			m.searchInput.Blur()
			q := m.searchInput.Value()
			m.viewport.SetSearchQuery(q)
			if len(m.viewport.MatchLines) > 0 {
				return m, m.showToast(fmt.Sprintf("/%s (%d coincidencias)", q, len(m.viewport.MatchLines)))
			}
			return m, m.showToast(fmt.Sprintf("Patrón no encontrado: %s", q))
		default:
			var cmd tea.Cmd
			m.searchInput, cmd = m.searchInput.Update(msg)
			return m, cmd
		}
	}

	// 3. Leader Key Sequence (`Space + <key>`)
	if m.leaderPending {
		m.leaderPending = false
		m.leaderWhichKeyVisible = false
		return m.handleLeaderFollowKey(msg)
	}

	// 4. Active Floating Overlay Handling
	switch m.effectiveOverlay() {
	case OverlayFuzzySwitcher:
		return m.handleFuzzyOverlayKey(msg)
	case OverlayInspectOutput:
		return m.handleInspectOverlayKey(msg)
	case OverlayWorkspaces:
		switch msg.String() {
		case "esc", "q", "enter":
			m.overlay = OverlayNone
		}
		return m, nil
	case OverlayKillConfirm:
		return m.handleKillConfirmKey(msg)
	case OverlayConsent:
		if handled, cmd := m.handleConsentModalKey(msg); handled {
			return m, cmd
		}
	}

	// 5. Standard Vim NORMAL Mode Keybindings
	switch msg.String() {
	case "ctrl+c", "q", "Q":
		m.Close()
		return m, tea.Quit

	case " ":
		m.leaderPending = true
		m.leaderWhichKeyVisible = false
		m.leaderSeq++
		return m, scheduleWhichKeyTimer(m.leaderSeq)

	case "i", "enter", "ctrl+t":
		if _, ok := m.activeTab(); ok {
			m.mode = ModeTerminal
			m.lastEscAt = time.Time{}
			return m, m.showToast("Modo TERMINAL (pulsa Esc Esc para volver a NORMAL)")
		}
		return m, nil

	case "h", "[", "shift+tab", "left":
		if len(m.tabs) > 0 {
			m.activeTabIdx = (m.activeTabIdx - 1 + len(m.tabs)) % len(m.tabs)
			m.viewport.AutoScroll = true
			m.syncActiveBufferLines()
		}
		return m, nil

	case "l", "]", "tab", "right":
		if len(m.tabs) > 0 {
			m.activeTabIdx = (m.activeTabIdx + 1) % len(m.tabs)
			m.viewport.AutoScroll = true
			m.syncActiveBufferLines()
		}
		return m, nil

	case "j", "down":
		m.viewport.ScrollDown(1)
		return m, nil

	case "k", "up":
		m.viewport.ScrollUp(1)
		return m, nil

	case "ctrl+d":
		m.viewport.ScrollDown(m.viewport.Height / 2)
		return m, nil

	case "ctrl+u":
		m.viewport.ScrollUp(m.viewport.Height / 2)
		return m, nil

	case "g":
		m.viewport.GotoTop()
		return m, m.showToast("Top of buffer (Auto-scroll pausado)")

	case "G":
		m.viewport.GotoBottom()
		return m, m.showToast("Bottom of buffer (Auto-scroll activo)")

	case "/":
		m.searchInputActive = true
		m.searchInput.SetValue("")
		m.searchInput.Focus()
		return m, textinput.Blink

	case "n":
		if m.viewport.NextMatch() {
			return m, m.showToast(fmt.Sprintf("Match %d/%d", m.viewport.CurrentMatch+1, len(m.viewport.MatchLines)))
		}
		return m, nil

	case "N":
		if m.viewport.PrevMatch() {
			return m, m.showToast(fmt.Sprintf("Match %d/%d", m.viewport.CurrentMatch+1, len(m.viewport.MatchLines)))
		}
		return m, nil

	case "y":
		var activeCmd string
		if tab, ok := m.activeTab(); ok {
			activeCmd = tab.ActiveCommand
		}
		count, err := m.viewport.YankLatestBlock(activeCmd)
		if err != nil {
			return m, m.showToast(err.Error())
		}
		return m, m.showToast(fmt.Sprintf("Yank: %d líneas copiadas al portapapeles", count))

	case "a", "A":
		return m, m.resolveSelectedConsent(bus.DecisionAllowOnce, "Aprobado 1x")

	case "s", "S":
		return m, m.resolveSelectedConsent(bus.DecisionAllowSession, "Aprobado en sesión")

	case "d", "D":
		return m, m.resolveSelectedConsent(bus.DecisionDeny, "Denegado")
	}

	return m, nil
}

func (m *Model) handleLeaderFollowKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case " ", "t":
		m.overlay = OverlayFuzzySwitcher
		m.fuzzyInput.SetValue("")
		m.fuzzyCursor = 0
		m.fuzzyInput.Focus()
		return m, textinput.Blink

	case "a":
		if m.overlay == OverlayConsent {
			m.overlay = OverlayNone
			m.consentDismissed = true
		} else {
			m.consentDismissed = false
			m.overlay = OverlayConsent
		}
		return m, nil

	case "o":
		m.syncActiveBufferLines()
		m.inspectLines = append([]string(nil), m.viewport.Lines...)
		m.inspectScroll = m.viewport.TopLine
		m.overlay = OverlayInspectOutput
		return m, nil

	case "w":
		m.overlay = OverlayWorkspaces
		return m, nil

	case "z":
		m.zenMode = !m.zenMode
		m.recalculateLayout()
		if m.zenMode {
			return m, m.showToast("Zen Mode: ON")
		}
		return m, m.showToast("Zen Mode: OFF")

	case "k":
		if _, ok := m.activeTab(); ok {
			m.overlay = OverlayKillConfirm
		} else {
			return m, m.showToast("Sin pestaña activa para terminar")
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) handleConsentModalKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	pending := m.eventBus.PendingConsents()
	if len(pending) == 0 {
		m.overlay = OverlayNone
		return false, nil
	}

	switch msg.String() {
	case "a", "A":
		return true, m.resolveSelectedConsent(bus.DecisionAllowOnce, "Aprobado 1x")
	case "s", "S":
		return true, m.resolveSelectedConsent(bus.DecisionAllowSession, "Aprobado en sesión")
	case "d", "D":
		return true, m.resolveSelectedConsent(bus.DecisionDeny, "Denegado")
	case "tab":
		m.consentCursor = (m.consentCursor + 1) % len(pending)
		return true, nil
	case "shift+tab":
		m.consentCursor = (m.consentCursor - 1 + len(pending)) % len(pending)
		return true, nil
	case "esc":
		m.overlay = OverlayNone
		m.consentDismissed = true
		return true, nil
	}
	return false, nil
}

func (m *Model) resolveSelectedConsent(decision bus.ConsentDecision, label string) tea.Cmd {
	pending := m.eventBus.PendingConsents()
	if len(pending) == 0 {
		return nil
	}
	if m.consentCursor >= len(pending) {
		m.consentCursor = 0
	}
	target := pending[m.consentCursor]
	if err := m.eventBus.ResolveConsent(target.ID, decision, label); err != nil {
		return nil
	}
	if len(m.eventBus.PendingConsents()) == 0 && m.overlay == OverlayConsent {
		m.overlay = OverlayNone
	}
	return m.showToast(fmt.Sprintf("%s: %s", label, target.AgentID))
}

func (m *Model) handleFuzzyOverlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	filtered := m.filteredFuzzyTabs()
	switch msg.Type {
	case tea.KeyEsc:
		m.fuzzyInput.Blur()
		m.overlay = OverlayNone
		return m, nil
	case tea.KeyUp, tea.KeyCtrlP:
		if len(filtered) > 0 {
			m.fuzzyCursor = (m.fuzzyCursor - 1 + len(filtered)) % len(filtered)
		}
		return m, nil
	case tea.KeyDown, tea.KeyCtrlN, tea.KeyTab:
		if len(filtered) > 0 {
			m.fuzzyCursor = (m.fuzzyCursor + 1) % len(filtered)
		}
		return m, nil
	case tea.KeyEnter:
		if len(filtered) > 0 {
			if m.fuzzyCursor >= len(filtered) {
				m.fuzzyCursor = 0
			}
			m.activeTabIdx = filtered[m.fuzzyCursor].index
			m.viewport.AutoScroll = true
			m.syncActiveBufferLines()
		}
		m.fuzzyInput.Blur()
		m.overlay = OverlayNone
		return m, nil
	default:
		var cmd tea.Cmd
		m.fuzzyInput, cmd = m.fuzzyInput.Update(msg)
		m.fuzzyCursor = 0
		return m, cmd
	}
}

func (m *Model) handleInspectOverlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.overlay = OverlayNone
	case "j", "down":
		if m.inspectScroll+1 < len(m.inspectLines) {
			m.inspectScroll++
		}
	case "k", "up":
		if m.inspectScroll > 0 {
			m.inspectScroll--
		}
	case "ctrl+d":
		m.inspectScroll += 10
		if m.inspectScroll >= len(m.inspectLines) {
			m.inspectScroll = max(0, len(m.inspectLines)-1)
		}
	case "ctrl+u":
		m.inspectScroll = max(0, m.inspectScroll-10)
	case "g":
		m.inspectScroll = 0
	case "G":
		m.inspectScroll = max(0, len(m.inspectLines)-10)
	case "y":
		count, _ := m.viewport.YankLatestBlock("")
		return m, m.showToast(fmt.Sprintf("Yank: %d líneas copiadas", count))
	}
	return m, nil
}

func (m *Model) handleKillConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		m.overlay = OverlayNone
		if tab, ok := m.activeTab(); ok && m.ptyMgr != nil {
			_ = m.ptyMgr.CloseSession(tab.AgentID, tab.ID)
			m.syncTabs()
			m.syncActiveBufferLines()
			return m, m.showToast(fmt.Sprintf("Proceso terminado: %s", tab.Name))
		}
	case "n", "N", "esc", "q":
		m.overlay = OverlayNone
	}
	return m, nil
}

func (m *Model) handleBusEvent(evt bus.Event) {
	switch evt.Type {
	case bus.EventConsentQueued:
		m.consentDismissed = false
	case bus.EventSessionOpened:
		m.syncTabs()
		if evt.TerminalSession != nil {
			m.selectTabByID(evt.TerminalSession.ID)
		}
		m.syncActiveBufferLines()
	case bus.EventSessionUpdated, bus.EventSessionOutput, bus.EventSessionClosed:
		m.syncTabs()
		m.syncActiveBufferLines()
	}
}

func (m *Model) syncTabs() {
	if m.ptyMgr == nil {
		return
	}
	m.tabs = m.ptyMgr.AllSessions()
	if len(m.tabs) == 0 {
		m.activeTabIdx = 0
		m.mode = ModeNormal
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

func (m *Model) syncActiveBufferLines() {
	tab, ok := m.activeTab()
	if !ok || m.ptyMgr == nil {
		m.viewport.SetLines([]string{
			fmt.Sprintf("Tether Side-Car Server (%s) — Sin sesiones PTY activas.", m.listenAddr),
			"Los agentes conectados pueden abrir terminales mediante la tool MCP `session_open`.",
		})
		return
	}
	raw := m.ptyMgr.RawScreenForTUI(tab.AgentID, tab.ID)
	if strings.TrimSpace(raw) == "" {
		m.viewport.SetLines([]string{
			fmt.Sprintf("Sesión %s activa en %s (CWD: %s)", tab.Name, tab.WorkspaceRoot, tab.RelCWD),
		})
		return
	}
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	m.viewport.SetLines(lines)
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
	vpHeight := h - 2 // 1 line TopTabBar + 1 line BottomStatusline -> 95% central viewport
	if m.zenMode {
		vpHeight = h
	}
	if vpHeight < 4 {
		vpHeight = 4
	}
	m.viewport.SetSize(w, vpHeight)
}

func (m *Model) showToast(msg string) tea.Cmd {
	m.toastSeq++
	m.toastMessage = msg
	return scheduleToastClear(m.toastSeq, 2500*time.Millisecond)
}

// View renders the cached 60 FPS frame or recomputes it when the dirty flag is set.
func (m *Model) View() string {
	if m.dirty.Swap(false) || m.cachedView == "" {
		m.cachedView = m.composeFrame()
	}
	return m.cachedView
}

func (m *Model) composeFrame() string {
	dimBg := m.effectiveOverlay() != OverlayNone
	centerCanvas := m.viewport.RenderVirtualized(m.styles, dimBg)
	centerWithOverlay := m.RenderActiveOverlay(centerCanvas)

	if m.zenMode {
		return lipgloss.NewStyle().Background(BgBase).Render(centerWithOverlay)
	}

	topBar := m.RenderTopTabBar()
	bottomBar := m.RenderBottomStatusline()

	return fmt.Sprintf("%s\n%s\n%s", topBar, centerWithOverlay, bottomBar)
}
