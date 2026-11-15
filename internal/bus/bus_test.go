package bus

import (
	"context"
	"testing"
	"time"
)

func TestEventBus_PubSubAndFIFOConsent(t *testing.T) {
	eb := NewEventBus()
	eventsCh, unsub := eb.Subscribe(16)
	defer unsub()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type outcome struct {
		res ConsentResult
		err error
	}
	ch1 := make(chan outcome, 1)
	ch2 := make(chan outcome, 1)

	go func() {
		res, err := eb.RequestConsent(ctx, ConsentRequest{
			ID:        "req-1",
			Kind:      ConsentKindConnect,
			AgentID:   "agent-1",
			AgentName: "Planner",
			Workspace: "/workspace/a",
		})
		ch1 <- outcome{res: res, err: err}
	}()

	// Wait for req-1 to be queued
	evt1 := <-eventsCh
	if evt1.Type != EventConsentQueued || evt1.ConsentRequest.ID != "req-1" {
		t.Fatalf("expected EventConsentQueued for req-1, got %+v", evt1)
	}

	go func() {
		res, err := eb.RequestConsent(ctx, ConsentRequest{
			ID:        "req-2",
			Kind:      ConsentKindCommand,
			AgentID:   "agent-2",
			AgentName: "Coder",
			Command:   "rm -rf build",
		})
		ch2 <- outcome{res: res, err: err}
	}()

	evt2 := <-eventsCh
	if evt2.Type != EventConsentQueued || evt2.ConsentRequest.ID != "req-2" {
		t.Fatalf("expected EventConsentQueued for req-2, got %+v", evt2)
	}

	pending := eb.PendingConsents()
	if len(pending) != 2 || pending[0].ID != "req-1" || pending[1].ID != "req-2" {
		t.Fatalf("expected FIFO order [req-1, req-2], got %+v", pending)
	}

	// Resolve FIFO head (req-1)
	resolvedReq, err := eb.ResolveNextConsent(DecisionAllowOnce, "approved by operator")
	if err != nil {
		t.Fatalf("ResolveNextConsent failed: %v", err)
	}
	if resolvedReq.ID != "req-1" {
		t.Fatalf("expected req-1 resolved first, got %s", resolvedReq.ID)
	}

	out1 := <-ch1
	if out1.err != nil || out1.res.Decision != DecisionAllowOnce {
		t.Fatalf("unexpected outcome for req-1: %+v", out1)
	}

	// Resolve second item (req-2)
	if err := eb.ResolveConsent("req-2", DecisionDeny, "denied"); err != nil {
		t.Fatalf("ResolveConsent failed: %v", err)
	}
	out2 := <-ch2
	if out2.err != nil || out2.res.Decision != DecisionDeny {
		t.Fatalf("unexpected outcome for req-2: %+v", out2)
	}

	if len(eb.PendingConsents()) != 0 {
		t.Fatalf("expected empty consent queue after resolving all items")
	}
}

func TestEventBus_ConsentCancelCleansQueue(t *testing.T) {
	eb := NewEventBus()
	eventsCh, unsub := eb.Subscribe(16)
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := eb.RequestConsent(ctx, ConsentRequest{
			ID:      "req-cancel",
			Kind:    ConsentKindConnect,
			AgentID: "agent-cancel",
		})
		errCh <- err
	}()

	evt := <-eventsCh
	if evt.Type != EventConsentQueued {
		t.Fatalf("expected EventConsentQueued, got %v", evt.Type)
	}

	if len(eb.PendingConsents()) != 1 {
		t.Fatalf("expected 1 pending request in queue")
	}

	cancel()

	if err := <-errCh; err != ErrConsentCancelled {
		t.Fatalf("expected ErrConsentCancelled, got %v", err)
	}

	if len(eb.PendingConsents()) != 0 {
		t.Fatalf("expected cancelled request to be removed from pending queue")
	}
}
