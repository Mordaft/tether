package pty

import (
	"regexp"
	"strings"
)

var (
	// Matches ANSI CSI sequences (\x1b[ ...), OSC sequences (\x1b] ... BEL/ST),
	// DCS/PM/APC sequences, and 2-byte ESC sequences.
	ansiRegex = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[P^_][^\x1b]*\x1b\\|[@-Z\\-_])`)

	// Matches internal Tether execution sentinel lines so they never leak into agent or TUI output.
	sentinelLineRegex = regexp.MustCompile(`(?m)^[^\r\n]*__TETHER_DONE_[a-f0-9]+__:[-\d]+__[^\r\n]*\r?\n?`)
)

// StripANSI removes ANSI escape sequences, terminal title OSC codes, and control noise
// from terminal output so SLMs/LLMs receive clean text without context bloat.
func StripANSI(input string) string {
	if input == "" {
		return ""
	}
	cleaned := ansiRegex.ReplaceAllString(input, "")
	cleaned = sentinelLineRegex.ReplaceAllString(cleaned, "")
	// Normalize CRLF -> LF
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	// Remove standalone carriage returns and bell characters
	cleaned = strings.ReplaceAll(cleaned, "\r", "")
	cleaned = strings.ReplaceAll(cleaned, "\x07", "")
	// Apply backspaces (\x08) if present
	if strings.ContainsRune(cleaned, '\x08') {
		var buf []rune
		for _, r := range cleaned {
			if r == '\x08' {
				if len(buf) > 0 && buf[len(buf)-1] != '\n' {
					buf = buf[:len(buf)-1]
				}
			} else {
				buf = append(buf, r)
			}
		}
		cleaned = string(buf)
	}
	return cleaned
}
