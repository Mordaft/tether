package tui

import (
	"bytes"
	"strings"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/lipgloss"

	"tether/internal/bus"
)

// Tether Night color palette tokens (Tokyo Night Storm inspired).
const (
	BgBase       = lipgloss.Color("#1a1b26")
	BgFloat      = lipgloss.Color("#24283b")
	BgSurface    = lipgloss.Color("#292e42")
	BorderFocus  = lipgloss.Color("#7aa2f7")
	BorderMuted  = lipgloss.Color("#414868")
	FgPrimary    = lipgloss.Color("#c0caf5")
	FgMuted      = lipgloss.Color("#565f89")
	Primary      = lipgloss.Color("#7aa2f7")
	Secondary    = lipgloss.Color("#bb9af7")
	AccentNormal = lipgloss.Color("#9ece6a")
	AccentTerm   = lipgloss.Color("#ff9e64")
	AccentWarn   = lipgloss.Color("#e0af68")
	AccentDanger = lipgloss.Color("#f7768e")
)

// IconSet provides semantic glyphs using Nerd Fonts v3 or clean ASCII fallbacks (--no-icons).
type IconSet struct {
	NoIcons   bool
	Agent     string
	Dir       string
	Git       string
	TTL       string
	ShellBash string
	ShellPS   string
	ShellCMD  string
	StateRun  string
	StateOK   string
	StateWait string
	StatePerm string
}

// NewIconSet returns the Nerd Font v3 glyph dictionary or ASCII labels when noIcons is true.
func NewIconSet(noIcons bool) IconSet {
	if noIcons {
		return IconSet{
			NoIcons:   true,
			Agent:     "[AG]",
			Dir:       "[DIR]",
			Git:       "[GIT]",
			TTL:       "[TTL]",
			ShellBash: "[SH]",
			ShellPS:   "[PS]",
			ShellCMD:  "[CMD]",
			StateRun:  "[RUN]",
			StateOK:   "[OK]",
			StateWait: "[WAIT]",
			StatePerm: "[PERM]",
		}
	}
	return IconSet{
		NoIcons:   false,
		Agent:     "󰚩",
		Dir:       "",
		Git:       "",
		TTL:       "⏱",
		ShellBash: "",
		ShellPS:   "󰨊",
		ShellCMD:  "",
		StateRun:  "",
		StateOK:   "",
		StateWait: "",
		StatePerm: "",
	}
}

// ForShell returns the semantic icon corresponding to a shell interpreter name.
func (ic IconSet) ForShell(shell string) string {
	s := strings.ToLower(shell)
	switch {
	case strings.Contains(s, "pwsh") || strings.Contains(s, "powershell"):
		return ic.ShellPS
	case strings.Contains(s, "cmd") || strings.Contains(s, "git-bash"):
		return ic.ShellCMD
	default:
		return ic.ShellBash
	}
}

// ForState returns the semantic icon corresponding to a PTY session state.
func (ic IconSet) ForState(state bus.SessionState) string {
	switch state {
	case bus.StateRunning:
		return ic.StateRun
	case bus.StateWaitingForInput:
		return ic.StateWait
	default:
		return ic.StateOK
	}
}

// Styles holds precomputed lipgloss styles following the Tether Night hierarchy.
type Styles struct {
	BaseCanvas      lipgloss.Style
	ModalTitle      lipgloss.Style
	TabActive       lipgloss.Style
	TabInactive     lipgloss.Style
	TabWaiting      lipgloss.Style
	BadgePending    lipgloss.Style
	CommandBold     lipgloss.Style
	MetadataItalic  lipgloss.Style
	TimestampFaint  lipgloss.Style
	LineNumberFaint lipgloss.Style
	FloatWindow     lipgloss.Style
	FloatWarn       lipgloss.Style
	FloatDanger     lipgloss.Style
	ModeNormal      lipgloss.Style
	ModeTerminal    lipgloss.Style
	StatusAgent     lipgloss.Style
	StatusSegment   lipgloss.Style
	StatusLeader    lipgloss.Style
	MatchHighlight  lipgloss.Style
	ToastSuccess    lipgloss.Style
}

// NewStyles constructs the Tether Night lipgloss style hierarchy.
func NewStyles() Styles {
	return Styles{
		BaseCanvas: lipgloss.NewStyle().
			Background(BgBase).
			Foreground(FgPrimary),

		ModalTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(Primary).
			Background(BgFloat),

		TabActive: lipgloss.NewStyle().
			Bold(true).
			Foreground(BgBase).
			Background(Primary).
			Padding(0, 1),

		TabInactive: lipgloss.NewStyle().
			Foreground(FgMuted).
			Background(BgSurface).
			Padding(0, 1),

		TabWaiting: lipgloss.NewStyle().
			Bold(true).
			Foreground(BgBase).
			Background(AccentWarn).
			Padding(0, 1),

		BadgePending: lipgloss.NewStyle().
			Bold(true).
			Foreground(BgBase).
			Background(AccentWarn).
			Padding(0, 1),

		CommandBold: lipgloss.NewStyle().
			Bold(true).
			Foreground(FgPrimary),

		MetadataItalic: lipgloss.NewStyle().
			Italic(true).
			Foreground(FgMuted),

		TimestampFaint: lipgloss.NewStyle().
			Faint(true).
			Foreground(FgMuted),

		LineNumberFaint: lipgloss.NewStyle().
			Faint(true).
			Foreground(FgMuted),

		FloatWindow: lipgloss.NewStyle().
			Background(BgFloat).
			Foreground(FgPrimary).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(BorderFocus).
			Padding(1, 2),

		FloatWarn: lipgloss.NewStyle().
			Background(BgFloat).
			Foreground(FgPrimary).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(AccentWarn).
			Padding(1, 2),

		FloatDanger: lipgloss.NewStyle().
			Background(BgFloat).
			Foreground(FgPrimary).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(AccentDanger).
			Padding(1, 2),

		ModeNormal: lipgloss.NewStyle().
			Bold(true).
			Foreground(BgBase).
			Background(AccentNormal).
			Padding(0, 1),

		ModeTerminal: lipgloss.NewStyle().
			Bold(true).
			Foreground(BgBase).
			Background(AccentTerm).
			Padding(0, 1),

		StatusAgent: lipgloss.NewStyle().
			Bold(true).
			Foreground(Secondary).
			Background(BgSurface).
			Padding(0, 1),

		StatusSegment: lipgloss.NewStyle().
			Foreground(FgPrimary).
			Background(BgSurface).
			Padding(0, 1),

		StatusLeader: lipgloss.NewStyle().
			Bold(true).
			Foreground(Primary).
			Background(BgSurface).
			Padding(0, 1),

		MatchHighlight: lipgloss.NewStyle().
			Bold(true).
			Foreground(BgBase).
			Background(AccentWarn),

		ToastSuccess: lipgloss.NewStyle().
			Bold(true).
			Foreground(BgBase).
			Background(AccentNormal).
			Padding(0, 1),
	}
}

// HighlightCommand applies Chroma syntax highlighting (Tokyo Night Storm / terminal16m)
// to a shell command or structured output snippet.
func HighlightCommand(code string, lexer string) string {
	if strings.TrimSpace(code) == "" {
		return ""
	}
	if lexer == "" {
		lexer = "bash"
	}
	var buf bytes.Buffer
	if err := quick.Highlight(&buf, code, lexer, "terminal16m", "tokyonight-storm"); err != nil {
		return lipgloss.NewStyle().Bold(true).Foreground(FgPrimary).Render(code)
	}
	return strings.TrimRight(buf.String(), "\r\n")
}

// DetectOutputLexer guesses an appropriate Chroma lexer for the Inspect Output floating window.
func DetectOutputLexer(content string) string {
	trimmed := strings.TrimSpace(content)
	switch {
	case strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "["):
		return "json"
	case strings.Contains(trimmed, "diff --git") || strings.Contains(trimmed, "@@ -"):
		return "diff"
	default:
		return "bash"
	}
}
