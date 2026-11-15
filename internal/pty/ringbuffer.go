package pty

import (
	"strings"
	"sync"
)

const DefaultRingBufferCapacity = 256 * 1024 // 256 KB per session

// RingBuffer is a thread-safe circular byte buffer that retains the most recent
// capacity bytes of PTY output and supports offset-based slicing for command execution.
type RingBuffer struct {
	mu           sync.RWMutex
	buf          []byte
	capacity     int
	totalWritten int64
}

// NewRingBuffer creates a new RingBuffer with the given byte capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = DefaultRingBufferCapacity
	}
	return &RingBuffer{
		buf:      make([]byte, 0, capacity),
		capacity: capacity,
	}
}

// Write appends p to the buffer, evicting the oldest bytes if capacity is exceeded.
func (rb *RingBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.totalWritten += int64(len(p))

	if len(p) >= rb.capacity {
		rb.buf = append(rb.buf[:0], p[len(p)-rb.capacity:]...)
		return len(p), nil
	}

	overflow := len(rb.buf) + len(p) - rb.capacity
	if overflow > 0 {
		rb.buf = append(rb.buf[:0], rb.buf[overflow:]...)
	}
	rb.buf = append(rb.buf, p...)
	return len(p), nil
}

// TotalWritten returns the monotonic count of all bytes ever written to this buffer.
func (rb *RingBuffer) TotalWritten() int64 {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.totalWritten
}

// ReadSince returns the bytes written since startOffset (or the available tail if
// more than capacity bytes were written since startOffset).
func (rb *RingBuffer) ReadSince(startOffset int64) string {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if startOffset >= rb.totalWritten {
		return ""
	}
	earliestOffset := rb.totalWritten - int64(len(rb.buf))
	if startOffset <= earliestOffset {
		return string(rb.buf)
	}
	relIdx := int(startOffset - earliestOffset)
	return string(rb.buf[relIdx:])
}

// String returns the entire retained content of the ring buffer.
func (rb *RingBuffer) String() string {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return string(rb.buf)
}

// LastCleanLines strips ANSI sequences from the buffer content and returns the last n lines.
func (rb *RingBuffer) LastCleanLines(n int) []string {
	raw := rb.String()
	clean := StripANSI(raw)
	if clean == "" {
		return nil
	}

	clean = strings.TrimRight(clean, "\n")
	lines := strings.Split(clean, "\n")
	if n <= 0 || n >= len(lines) {
		return lines
	}
	return lines[len(lines)-n:]
}
