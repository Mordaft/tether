package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	// LeaderWhichKeyDelay is the 300ms threshold after pressing Space in NORMAL mode
	// before popping up the bottom-right Which-Key floating guide.
	LeaderWhichKeyDelay = 300 * time.Millisecond

	// DoubleEscThreshold is the maximum interval between two consecutive Esc presses
	// in TERMINAL mode (`Esc Esc`) to return to NORMAL mode.
	DoubleEscThreshold = 450 * time.Millisecond
)

// WhichKeyTimeoutMsg fires 300ms after the user presses the Space leader key.
type WhichKeyTimeoutMsg struct {
	Seq uint64
}

// ToastExpireMsg clears a temporary status toast notification.
type ToastExpireMsg struct {
	Seq uint64
}

// WhichKeyEntry describes a shortcut displayed in the dynamic Which-Key popup.
type WhichKeyEntry struct {
	Key  string
	Desc string
}

// LeaderMenuEntries lists the official Leader (Space) bindings.
var LeaderMenuEntries = []WhichKeyEntry{
	{Key: "Space / t", Desc: "Session Switcher (Telescope Fuzzy)"},
	{Key: "a", Desc: "Approvals Queue (Consent Modal)"},
	{Key: "o", Desc: "Inspect Output (Chroma Viewer)"},
	{Key: "w", Desc: "Workspaces & Agents (TTL Info)"},
	{Key: "z", Desc: "Zen / Zoom Mode (Toggle Bars)"},
	{Key: "k", Desc: "Kill Process Group (Confirm)"},
}

func scheduleWhichKeyTimer(seq uint64) tea.Cmd {
	return tea.Tick(LeaderWhichKeyDelay, func(time.Time) tea.Msg {
		return WhichKeyTimeoutMsg{Seq: seq}
	})
}

func scheduleToastClear(seq uint64, d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return ToastExpireMsg{Seq: seq}
	})
}

// KeyMsgToRawBytes converts a Bubbletea tea.KeyMsg into raw terminal bytes
// suitable for direct forwarding to a PTY in TERMINAL/INSERT mode.
func KeyMsgToRawBytes(msg tea.KeyMsg) []byte {
	switch msg.Type {
	case tea.KeyEnter:
		return []byte("\r")
	case tea.KeyTab:
		return []byte("\t")
	case tea.KeyBackspace:
		return []byte{0x7f}
	case tea.KeySpace:
		return []byte(" ")
	case tea.KeyEsc:
		return []byte{0x1b}
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyCtrlC:
		return []byte{0x03}
	case tea.KeyCtrlD:
		return []byte{0x04}
	case tea.KeyCtrlZ:
		return []byte{0x1a}
	case tea.KeyCtrlL:
		return []byte{0x0c}
	case tea.KeyCtrlU:
		return []byte{0x15}
	case tea.KeyCtrlW:
		return []byte{0x17}
	case tea.KeyCtrlA:
		return []byte{0x01}
	case tea.KeyCtrlE:
		return []byte{0x05}
	case tea.KeyCtrlK:
		return []byte{0x0b}
	case tea.KeyRunes:
		return []byte(string(msg.Runes))
	default:
		s := msg.String()
		if len(s) == 1 {
			return []byte(s)
		}
		return nil
	}
}
