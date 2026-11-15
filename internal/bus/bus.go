package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var (
	// ErrConsentCancelled is returned when the caller's context expires before human approval.
	ErrConsentCancelled = errors.New("consent request cancelled or timed out")
	// ErrConsentNotFound is returned when attempting to resolve a non-existent consent request.
	ErrConsentNotFound = errors.New("consent request not found")
)

// EventBus coordinates asynchronous Pub/Sub events and the centralized FIFO human
// consent queue between HTTP/MCP handlers, PTY sessions, and the Bubbletea TUI.
type EventBus struct {
	mu           sync.RWMutex
	nextSubID    uint64
	subscribers  map[uint64]chan Event
	consentQueue []*ConsentRequest
}

// NewEventBus creates a new ready-to-use EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers:  make(map[uint64]chan Event),
		consentQueue: make([]*ConsentRequest, 0),
	}
}

// Subscribe registers a listener channel for system events and returns a receive-only
// channel along with an unsubscribe function.
func (b *EventBus) Subscribe(bufferSize int) (<-chan Event, func()) {
	if bufferSize <= 0 {
		bufferSize = 64
	}
	ch := make(chan Event, bufferSize)

	b.mu.Lock()
	b.nextSubID++
	id := b.nextSubID
	b.subscribers[id] = ch
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			if subCh, ok := b.subscribers[id]; ok {
				delete(b.subscribers, id)
				close(subCh)
			}
			b.mu.Unlock()
		})
	}

	return ch, unsubscribe
}

// Publish broadcasts an event non-blockingly to all active subscribers.
func (b *EventBus) Publish(evt Event) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subscribers {
		select {
		case ch <- evt:
		default:
			// Drop oldest event if subscriber buffer is full so HTTP/PTY never deadlocks.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- evt:
			default:
			}
		}
	}
}

// RequestConsent enqueues a ConsentRequest in the centralized FIFO queue and blocks
// until a human operator resolves it in the TUI or ctx is cancelled.
func (b *EventBus) RequestConsent(ctx context.Context, req ConsentRequest) (ConsentResult, error) {
	if req.ID == "" {
		req.ID = generateRequestID()
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now()
	}
	req.ResponseCh = make(chan ConsentResult, 1)

	reqCopy := req
	b.mu.Lock()
	b.consentQueue = append(b.consentQueue, &reqCopy)
	b.mu.Unlock()

	snapshot := reqCopy
	b.Publish(Event{
		Type:           EventConsentQueued,
		Timestamp:      reqCopy.CreatedAt,
		ConsentRequest: &snapshot,
	})

	select {
	case res := <-reqCopy.ResponseCh:
		return res, nil
	case <-ctx.Done():
		removed := b.removeConsentByID(reqCopy.ID)
		if removed != nil {
			b.Publish(Event{
				Type:           EventConsentCancelled,
				Timestamp:      time.Now(),
				ConsentRequest: removed,
			})
		}
		return ConsentResult{}, ErrConsentCancelled
	}
}

// PendingConsents returns a snapshot slice of all currently pending ConsentRequests in FIFO order.
func (b *EventBus) PendingConsents() []ConsentRequest {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]ConsentRequest, len(b.consentQueue))
	for i, req := range b.consentQueue {
		out[i] = *req
	}
	return out
}

// ResolveConsent resolves a specific pending ConsentRequest by ID.
func (b *EventBus) ResolveConsent(id string, decision ConsentDecision, reason string) error {
	b.mu.Lock()
	var target *ConsentRequest
	for i, req := range b.consentQueue {
		if req.ID == id {
			target = req
			b.consentQueue = append(b.consentQueue[:i], b.consentQueue[i+1:]...)
			break
		}
	}
	b.mu.Unlock()

	if target == nil {
		return ErrConsentNotFound
	}

	res := ConsentResult{
		Decision:   decision,
		Reason:     reason,
		ResolvedAt: time.Now(),
	}

	select {
	case target.ResponseCh <- res:
	default:
	}

	targetCopy := *target
	b.Publish(Event{
		Type:           EventConsentResolved,
		Timestamp:      res.ResolvedAt,
		ConsentRequest: &targetCopy,
		ConsentResult:  &res,
	})
	return nil
}

// ResolveNextConsent pops and resolves the oldest (FIFO head) ConsentRequest in the queue.
func (b *EventBus) ResolveNextConsent(decision ConsentDecision, reason string) (*ConsentRequest, error) {
	b.mu.Lock()
	if len(b.consentQueue) == 0 {
		b.mu.Unlock()
		return nil, ErrConsentNotFound
	}
	target := b.consentQueue[0]
	b.consentQueue = b.consentQueue[1:]
	b.mu.Unlock()

	res := ConsentResult{
		Decision:   decision,
		Reason:     reason,
		ResolvedAt: time.Now(),
	}

	select {
	case target.ResponseCh <- res:
	default:
	}

	targetCopy := *target
	b.Publish(Event{
		Type:           EventConsentResolved,
		Timestamp:      res.ResolvedAt,
		ConsentRequest: &targetCopy,
		ConsentResult:  &res,
	})
	return &targetCopy, nil
}

func (b *EventBus) removeConsentByID(id string) *ConsentRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, req := range b.consentQueue {
		if req.ID == id {
			b.consentQueue = append(b.consentQueue[:i], b.consentQueue[i+1:]...)
			cp := *req
			return &cp
		}
	}
	return nil
}

func generateRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "req-fallback"
	}
	return "req-" + hex.EncodeToString(buf[:])
}
