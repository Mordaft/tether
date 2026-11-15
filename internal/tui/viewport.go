package tui

import (
	"fmt"
	"strings"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/lipgloss"
)

// VirtualViewport manages virtualized line rendering, auto-scroll freezing,
// in-buffer search (/ + n/N), and clipboard yanking (y).
type VirtualViewport struct {
	Width        int
	Height       int
	Lines        []string
	TopLine      int
	AutoScroll   bool
	SearchQuery  string
	MatchLines   []int
	CurrentMatch int
}

// NewVirtualViewport creates a VirtualViewport with AutoScroll enabled by default.
func NewVirtualViewport(width, height int) VirtualViewport {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 24
	}
	return VirtualViewport{
		Width:      width,
		Height:     height,
		AutoScroll: true,
	}
}

// SetLines updates the underlying line slice and adjusts TopLine if AutoScroll is active.
func (vp *VirtualViewport) SetLines(lines []string) {
	vp.Lines = lines
	if vp.SearchQuery != "" {
		vp.recomputeMatches()
	}
	if vp.AutoScroll {
		vp.clampToBottom()
	} else {
		vp.clampBounds()
	}
}

// SetSize updates the viewport dimensions.
func (vp *VirtualViewport) SetSize(width, height int) {
	if width < 20 {
		width = 20
	}
	if height < 4 {
		height = 4
	}
	vp.Width = width
	vp.Height = height
	if vp.AutoScroll {
		vp.clampToBottom()
	} else {
		vp.clampBounds()
	}
}

// ScrollUp moves the viewport up by delta lines and freezes AutoScroll.
func (vp *VirtualViewport) ScrollUp(delta int) {
	if delta <= 0 {
		delta = 1
	}
	vp.AutoScroll = false
	vp.TopLine -= delta
	if vp.TopLine < 0 {
		vp.TopLine = 0
	}
}

// ScrollDown moves the viewport down by delta lines and re-enables AutoScroll if reaching bottom.
func (vp *VirtualViewport) ScrollDown(delta int) {
	if delta <= 0 {
		delta = 1
	}
	vp.TopLine += delta
	maxTop := vp.maxTopLine()
	if vp.TopLine >= maxTop {
		vp.TopLine = maxTop
		vp.AutoScroll = true
	}
}

// GotoTop jumps to the first line of the buffer and freezes AutoScroll.
func (vp *VirtualViewport) GotoTop() {
	vp.TopLine = 0
	vp.AutoScroll = false
}

// GotoBottom jumps to the end of the buffer and re-enables AutoScroll.
func (vp *VirtualViewport) GotoBottom() {
	vp.clampToBottom()
	vp.AutoScroll = true
}

// SetSearchQuery sets the active search query and jumps to the most recent match.
func (vp *VirtualViewport) SetSearchQuery(query string) {
	vp.SearchQuery = strings.TrimSpace(query)
	vp.recomputeMatches()
	if len(vp.MatchLines) > 0 {
		vp.CurrentMatch = len(vp.MatchLines) - 1
		vp.centerOnLine(vp.MatchLines[vp.CurrentMatch])
	}
}

// NextMatch jumps forward to the next search match (n).
func (vp *VirtualViewport) NextMatch() bool {
	if len(vp.MatchLines) == 0 {
		return false
	}
	vp.CurrentMatch = (vp.CurrentMatch + 1) % len(vp.MatchLines)
	vp.centerOnLine(vp.MatchLines[vp.CurrentMatch])
	return true
}

// PrevMatch jumps backward to the previous search match (N).
func (vp *VirtualViewport) PrevMatch() bool {
	if len(vp.MatchLines) == 0 {
		return false
	}
	vp.CurrentMatch = (vp.CurrentMatch - 1 + len(vp.MatchLines)) % len(vp.MatchLines)
	vp.centerOnLine(vp.MatchLines[vp.CurrentMatch])
	return true
}

func (vp *VirtualViewport) recomputeMatches() {
	vp.MatchLines = nil
	if vp.SearchQuery == "" {
		return
	}
	needle := strings.ToLower(vp.SearchQuery)
	for i, line := range vp.Lines {
		if strings.Contains(strings.ToLower(line), needle) {
			vp.MatchLines = append(vp.MatchLines, i)
		}
	}
	if vp.CurrentMatch >= len(vp.MatchLines) {
		vp.CurrentMatch = 0
	}
}

func (vp *VirtualViewport) centerOnLine(lineIdx int) {
	vp.AutoScroll = false
	targetTop := lineIdx - (vp.Height / 2)
	if targetTop < 0 {
		targetTop = 0
	}
	vp.TopLine = targetTop
	vp.clampBounds()
}

func (vp *VirtualViewport) maxTopLine() int {
	m := len(vp.Lines) - vp.Height
	if m < 0 {
		return 0
	}
	return m
}

func (vp *VirtualViewport) clampToBottom() {
	vp.TopLine = vp.maxTopLine()
}

func (vp *VirtualViewport) clampBounds() {
	if vp.TopLine < 0 {
		vp.TopLine = 0
	}
	maxTop := vp.maxTopLine()
	if vp.TopLine > maxTop {
		vp.TopLine = maxTop
	}
}

// RenderVirtualized formats ONLY the visible slice [TopLine : TopLine+Height] of lines,
// applying Tether Night styles, faint line numbers, search match highlights, and optional
// background dimming when a floating window overlay is active.
func (vp *VirtualViewport) RenderVirtualized(styles Styles, dimBackground bool) string {
	if vp.Height <= 0 {
		return ""
	}

	total := len(vp.Lines)
	start := vp.TopLine
	if start > total {
		start = total
	}
	end := start + vp.Height
	if end > total {
		end = total
	}

	visible := vp.Lines[start:end]
	outRows := make([]string, 0, vp.Height)

	lineStyle := lipgloss.NewStyle().Foreground(FgPrimary)
	if dimBackground {
		lineStyle = lineStyle.Faint(true).Foreground(FgMuted)
	}

	contentWidth := vp.Width - 7
	if contentWidth < 10 {
		contentWidth = 10
	}

	for i, rawLine := range visible {
		absLineNum := start + i + 1
		gutter := styles.LineNumberFaint.Render(fmt.Sprintf("%4d │ ", absLineNum))

		displayLine := truncateOrPadRunes(rawLine, contentWidth)
		var renderedText string
		if !dimBackground && vp.SearchQuery != "" {
			renderedText = highlightSearchTerm(displayLine, vp.SearchQuery, lineStyle, styles.MatchHighlight)
		} else {
			renderedText = lineStyle.Render(displayLine)
		}
		outRows = append(outRows, gutter+renderedText)
	}

	// Fill remaining viewport rows with subtle tilde lines (Neovim empty buffer style)
	tildeStyle := styles.LineNumberFaint
	for len(outRows) < vp.Height {
		outRows = append(outRows, tildeStyle.Render("   ~ │ ")+strings.Repeat(" ", contentWidth))
	}

	return strings.Join(outRows, "\n")
}

// YankLatestBlock copies the latest output block (up to 40 recent non-empty lines)
// to the system clipboard (with graceful fallback in headless/CI environments).
func (vp *VirtualViewport) YankLatestBlock(activeCmd string) (int, error) {
	var nonEmpty []string
	for i := len(vp.Lines) - 1; i >= 0 && len(nonEmpty) < 40; i-- {
		trimmed := strings.TrimRight(vp.Lines[i], " \t\r\n")
		if trimmed != "" {
			nonEmpty = append([]string{trimmed}, nonEmpty...)
		}
	}

	payload := strings.Join(nonEmpty, "\n")
	if payload == "" {
		payload = strings.TrimSpace(activeCmd)
	}
	if payload == "" {
		return 0, fmt.Errorf("buffer vacío; nada que copiar")
	}

	// Attempt system clipboard copy; ignore OS clipboard missing error in headless environments
	_ = clipboard.WriteAll(payload)
	return len(nonEmpty), nil
}

func highlightSearchTerm(line, query string, normalStyle, matchStyle lipgloss.Style) string {
	if query == "" {
		return normalStyle.Render(line)
	}
	lowerLine := strings.ToLower(line)
	lowerQuery := strings.ToLower(query)
	idx := strings.Index(lowerLine, lowerQuery)
	if idx == -1 {
		return normalStyle.Render(line)
	}

	var sb strings.Builder
	pos := 0
	qLen := len(query)
	for pos < len(line) {
		rel := strings.Index(lowerLine[pos:], lowerQuery)
		if rel == -1 {
			sb.WriteString(normalStyle.Render(line[pos:]))
			break
		}
		matchStart := pos + rel
		if matchStart > pos {
			sb.WriteString(normalStyle.Render(line[pos:matchStart]))
		}
		matchEnd := matchStart + qLen
		if matchEnd > len(line) {
			matchEnd = len(line)
		}
		sb.WriteString(matchStyle.Render(line[matchStart:matchEnd]))
		pos = matchEnd
	}
	return sb.String()
}

func truncateOrPadRunes(s string, width int) string {
	// Replace tabs with 4 spaces for deterministic column width
	s = strings.ReplaceAll(s, "\t", "    ")
	runes := []rune(s)
	if len(runes) > width {
		if width > 1 {
			return string(runes[:width-1]) + "…"
		}
		return string(runes[:width])
	}
	if len(runes) < width {
		return s + strings.Repeat(" ", width-len(runes))
	}
	return s
}
