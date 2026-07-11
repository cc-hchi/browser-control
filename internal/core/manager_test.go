package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cc-hchi/browser-control/internal/protocol"
)

func TestLeaseIsExclusiveRenewableAndExpires(t *testing.T) {
	m := newTestManager(t, 120*time.Millisecond)
	first := m.OpenSession("first", "test")
	second := m.OpenSession("second", "test")

	lease, rpcErr := m.ClaimTab(first.SessionID, "tab-1", 0)
	if rpcErr != nil {
		t.Fatalf("ClaimTab(first) error = %v", rpcErr)
	}
	if lease.SessionID != first.SessionID || lease.TabID != "tab-1" || lease.LeaseID == "" {
		t.Fatalf("ClaimTab(first) returned malformed lease: %+v", lease)
	}

	_, rpcErr = m.ClaimTab(second.SessionID, "tab-1", 0)
	assertRPCError(t, rpcErr, protocol.CodeLeaseConflict, "LEASE_CONFLICT")

	renewed, rpcErr := m.ClaimTab(first.SessionID, "tab-1", time.Hour)
	if rpcErr != nil {
		t.Fatalf("ClaimTab(same session) error = %v", rpcErr)
	}
	if renewed.LeaseID != lease.LeaseID {
		t.Fatalf("same-session claim changed lease ID: %q -> %q", lease.LeaseID, renewed.LeaseID)
	}
	if remaining := time.Until(renewed.ExpiresAt); remaining <= 0 || remaining > 150*time.Millisecond {
		t.Fatalf("renewed lease remaining TTL = %v, want clamped to manager TTL", remaining)
	}

	time.Sleep(160 * time.Millisecond)
	rpcErr = m.ValidateLease(first.SessionID, "tab-1", lease.LeaseID)
	assertRPCError(t, rpcErr, protocol.CodeLeaseExpired, "LEASE_EXPIRED")

	replacement, rpcErr := m.ClaimTab(second.SessionID, "tab-1", 0)
	if rpcErr != nil {
		t.Fatalf("ClaimTab(after expiry) error = %v", rpcErr)
	}
	if replacement.LeaseID == lease.LeaseID {
		t.Fatal("expired lease ID was reused")
	}
}

func TestLeaseReleaseRequiresOwnerAndCancelsTabQueue(t *testing.T) {
	m := newTestManager(t, time.Minute)
	owner := m.OpenSession("owner", "test")
	other := m.OpenSession("other", "test")
	lease, rpcErr := m.ClaimTab(owner.SessionID, "tab-1", 0)
	if rpcErr != nil {
		t.Fatalf("ClaimTab() error = %v", rpcErr)
	}

	_, rpcErr = m.ReleaseTab(other.SessionID, "tab-1", lease.LeaseID, "wrong owner")
	assertRPCError(t, rpcErr, protocol.CodeLeaseConflict, "LEASE_CONFLICT")

	activeStarted := make(chan struct{})
	type runResult struct{ err *protocol.RPCError }
	results := make(chan runResult, 2)
	go func() {
		_, err := m.RunTab(context.Background(), "tab-1", func(ctx context.Context) (json.RawMessage, *protocol.RPCError) {
			close(activeStarted)
			<-ctx.Done()
			return nil, cancelledError(ctx)
		})
		results <- runResult{err: err}
	}()
	awaitSignal(t, activeStarted, "active tab work")

	go func() {
		_, err := m.RunTab(context.Background(), "tab-1", func(context.Context) (json.RawMessage, *protocol.RPCError) {
			t.Error("queued browser work ran after lease release")
			return json.RawMessage(`{}`), nil
		})
		results <- runResult{err: err}
	}()
	waitForQueueLength(t, m, "tab-1", 1)

	if _, rpcErr := m.ReleaseTab(owner.SessionID, "tab-1", lease.LeaseID, "test release"); rpcErr != nil {
		t.Fatalf("ReleaseTab(owner) error = %v", rpcErr)
	}
	for range 2 {
		select {
		case result := <-results:
			assertRPCError(t, result.err, protocol.CodeCancelled, "CANCELLED")
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for cancelled tab work")
		}
	}
	if _, ok := m.LeaseForTab("tab-1"); ok {
		t.Fatal("released lease remains visible")
	}
}

func TestLeaseBrowserBindingCannotBeChanged(t *testing.T) {
	m := newTestManager(t, time.Minute)
	session := m.OpenSession("browser-binding", "test")
	lease, rpcErr := m.ClaimTab(session.SessionID, "tab-bound", 0)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	bound, rpcErr := m.BindLeaseBrowser(session.SessionID, "tab-bound", lease.LeaseID, "browser-a")
	if rpcErr != nil || bound.BrowserInstanceID != "browser-a" {
		t.Fatalf("initial binding = (%+v, %v)", bound, rpcErr)
	}
	_, rpcErr = m.BindLeaseBrowser(session.SessionID, "tab-bound", lease.LeaseID, "browser-b")
	assertRPCError(t, rpcErr, protocol.CodeLeaseConflict, "LEASE_CONFLICT")
	stored, ok := m.LeaseForTab("tab-bound")
	if !ok || stored.BrowserInstanceID != "browser-a" {
		t.Fatalf("binding changed after rejection: %+v, ok=%v", stored, ok)
	}
}

func TestSynchronousLeaseExpiryCancelsWorkAndInvokesReleaseHook(t *testing.T) {
	m := newTestManager(t, 40*time.Millisecond)
	session := m.OpenSession("expiry", "test")
	lease, rpcErr := m.ClaimTab(session.SessionID, "tab-expiry", 0)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	hooked := make(chan Lease, 1)
	m.SetLeaseExpiredHook(func(expired Lease) { hooked <- expired })
	started := make(chan struct{})
	done := make(chan *protocol.RPCError, 1)
	go func() {
		_, rpcErr := m.RunTab(context.Background(), "tab-expiry", func(ctx context.Context) (json.RawMessage, *protocol.RPCError) {
			close(started)
			<-ctx.Done()
			return nil, cancelledError(ctx)
		})
		done <- rpcErr
	}()
	awaitSignal(t, started, "expiring tab work")
	time.Sleep(50 * time.Millisecond)
	assertRPCError(t, m.ValidateLease(session.SessionID, "tab-expiry", lease.LeaseID), protocol.CodeLeaseExpired, "LEASE_EXPIRED")
	select {
	case rpcErr := <-done:
		assertRPCError(t, rpcErr, protocol.CodeCancelled, "CANCELLED")
	case <-time.After(time.Second):
		t.Fatal("expired lease did not cancel active work")
	}
	select {
	case expired := <-hooked:
		if expired.LeaseID != lease.LeaseID {
			t.Fatalf("hook lease = %+v", expired)
		}
	case <-time.After(time.Second):
		t.Fatal("expired lease did not invoke extension release hook")
	}
}

func TestTabQueueRunsFIFO(t *testing.T) {
	var q tabQueue
	var mu sync.Mutex
	order := make([]int, 0, 3)
	started := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	gates := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	done := make(chan *protocol.RPCError, 3)

	run := func(index int) {
		_, rpcErr := q.run(context.Background(), func(context.Context) (json.RawMessage, *protocol.RPCError) {
			mu.Lock()
			order = append(order, index)
			mu.Unlock()
			close(started[index-1])
			<-gates[index-1]
			return json.RawMessage(`{}`), nil
		})
		done <- rpcErr
	}

	go run(1)
	awaitSignal(t, started[0], "first FIFO item")
	go run(2)
	waitForWaiters(t, &q, 1)
	go run(3)
	waitForWaiters(t, &q, 2)

	close(gates[0])
	awaitSignal(t, started[1], "second FIFO item")
	close(gates[1])
	awaitSignal(t, started[2], "third FIFO item")
	close(gates[2])

	for range 3 {
		select {
		case rpcErr := <-done:
			if rpcErr != nil {
				t.Fatalf("q.run() error = %v", rpcErr)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for FIFO work")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := order, []int{1, 2, 3}; !equalInts(got, want) {
		t.Fatalf("execution order = %v, want %v", got, want)
	}
}

func TestExecuteOperationDeduplicatesConcurrentAndCompletedRequests(t *testing.T) {
	m := newTestManager(t, time.Minute)
	session := m.OpenSession("operations", "test")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	type result struct {
		value    json.RawMessage
		rpcErr   *protocol.RPCError
		replayed bool
	}
	results := make(chan result, 2)
	request := map[string]any{"kind": "click", "ref": "ref-1"}
	fn := func(context.Context) (json.RawMessage, *protocol.RPCError) {
		calls.Add(1)
		close(started)
		<-release
		return json.RawMessage(`{"clicked":true}`), nil
	}

	go func() {
		value, rpcErr, replayed := m.ExecuteOperation(context.Background(), session.SessionID, "tab-1", "op-1", request, fn)
		results <- result{value: value, rpcErr: rpcErr, replayed: replayed}
	}()
	awaitSignal(t, started, "first operation")
	go func() {
		value, rpcErr, replayed := m.ExecuteOperation(context.Background(), session.SessionID, "tab-1", "op-1", request, func(context.Context) (json.RawMessage, *protocol.RPCError) {
			t.Error("duplicate operation executed callback")
			return nil, nil
		})
		results <- result{value: value, rpcErr: rpcErr, replayed: replayed}
	}()
	close(release)

	seenReplay := map[bool]int{}
	for range 2 {
		select {
		case got := <-results:
			if got.rpcErr != nil {
				t.Fatalf("ExecuteOperation() error = %v", got.rpcErr)
			}
			if !bytes.Equal(got.value, json.RawMessage(`{"clicked":true}`)) {
				t.Fatalf("ExecuteOperation() value = %s", got.value)
			}
			seenReplay[got.replayed]++
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for operation results")
		}
	}
	if calls.Load() != 1 || seenReplay[false] != 1 || seenReplay[true] != 1 {
		t.Fatalf("calls = %d, replay flags = %v", calls.Load(), seenReplay)
	}

	value, rpcErr, replayed := m.ExecuteOperation(context.Background(), session.SessionID, "tab-1", "op-1", request, func(context.Context) (json.RawMessage, *protocol.RPCError) {
		t.Error("completed operation replay executed callback")
		return nil, nil
	})
	if rpcErr != nil || !replayed || !bytes.Equal(value, json.RawMessage(`{"clicked":true}`)) {
		t.Fatalf("completed replay = (%s, %v, %v)", value, rpcErr, replayed)
	}

	_, rpcErr, replayed = m.ExecuteOperation(context.Background(), session.SessionID, "tab-1", "op-1", map[string]any{"kind": "type"}, fn)
	if replayed {
		t.Fatal("conflicting operation was marked replayed")
	}
	assertRPCError(t, rpcErr, protocol.CodeOperationConflict, "INVALID_REQUEST")
}

func TestClosingSessionCancelsOperation(t *testing.T) {
	m := newTestManager(t, time.Minute)
	session := m.OpenSession("cancel", "test")
	started := make(chan struct{})
	done := make(chan *protocol.RPCError, 1)
	go func() {
		_, rpcErr, _ := m.ExecuteOperation(context.Background(), session.SessionID, "tab-1", "op-cancel", map[string]any{"kind": "wait"}, func(ctx context.Context) (json.RawMessage, *protocol.RPCError) {
			close(started)
			<-ctx.Done()
			return nil, cancelledError(ctx)
		})
		done <- rpcErr
	}()
	awaitSignal(t, started, "operation to cancel")
	if _, rpcErr := m.CloseSession(session.SessionID, "test close"); rpcErr != nil {
		t.Fatalf("CloseSession() error = %v", rpcErr)
	}
	select {
	case rpcErr := <-done:
		assertRPCError(t, rpcErr, protocol.CodeCancelled, "CANCELLED")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled operation")
	}
	view, rpcErr := m.GetOperation(session.SessionID, "op-cancel")
	if rpcErr != nil {
		t.Fatalf("GetOperation() error = %v", rpcErr)
	}
	if view.Status != "cancelled" || view.CompletedAt == nil {
		t.Fatalf("cancelled operation view = %+v", view)
	}
}

func TestExecuteOperationCanResumeAfterTrustedConfirmation(t *testing.T) {
	m := newTestManager(t, time.Minute)
	session := m.OpenSession("confirmation-resume", "test")
	request := map[string]any{"kind": "click", "target": "save"}
	_, rpcErr, replayed := m.ExecuteOperation(context.Background(), session.SessionID, "tab-1", "op-confirm", request, func(context.Context) (json.RawMessage, *protocol.RPCError) {
		return nil, protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "approve", true, map[string]any{"confirmationId": "conf-1"})
	})
	if replayed {
		t.Fatal("initial confirmation request was marked replayed")
	}
	assertRPCError(t, rpcErr, protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED")
	view, rpcErr := m.GetOperation(session.SessionID, "op-confirm")
	if rpcErr != nil || view.Status != "awaiting_confirmation" {
		t.Fatalf("awaiting operation = (%+v, %v)", view, rpcErr)
	}

	var calls atomic.Int32
	result, rpcErr, replayed := m.ExecuteOperation(context.Background(), session.SessionID, "tab-1", "op-confirm", request, func(context.Context) (json.RawMessage, *protocol.RPCError) {
		calls.Add(1)
		return json.RawMessage(`{"performed":true}`), nil
	})
	if rpcErr != nil || replayed || calls.Load() != 1 || !bytes.Equal(result, json.RawMessage(`{"performed":true}`)) {
		t.Fatalf("resumed operation = (%s, %v, replayed=%v, calls=%d)", result, rpcErr, replayed, calls.Load())
	}

	result, rpcErr, replayed = m.ExecuteOperation(context.Background(), session.SessionID, "tab-1", "op-confirm", request, func(context.Context) (json.RawMessage, *protocol.RPCError) {
		t.Fatal("completed confirmed operation executed twice")
		return nil, nil
	})
	if rpcErr != nil || !replayed || calls.Load() != 1 || !bytes.Equal(result, json.RawMessage(`{"performed":true}`)) {
		t.Fatalf("confirmed replay = (%s, %v, replayed=%v, calls=%d)", result, rpcErr, replayed, calls.Load())
	}
}

func TestDeniedConfirmationFinalizesAwaitingOperationAndEnforcesOwnership(t *testing.T) {
	m := newTestManager(t, time.Minute)
	owner := m.OpenSession("confirmation-owner", "test")
	other := m.OpenSession("confirmation-other", "test")
	request := map[string]any{"kind": "click", "target": "delete"}
	_, rpcErr, _ := m.ExecuteOperation(context.Background(), owner.SessionID, "tab-1", "op-denied", request, func(context.Context) (json.RawMessage, *protocol.RPCError) {
		return nil, protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "approve", true, nil)
	})
	assertRPCError(t, rpcErr, protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED")
	metadata := json.RawMessage(`{"operationId":"op-denied","requestHash":"ab00000000000000000000000000000000000000000000000000000000000000"}`)
	confirmation, rpcErr := m.CreateActionConfirmation(owner.SessionID, "tab-1", "browser-1", metadata, time.Minute)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if _, rpcErr := m.ResolveConfirmation(confirmation.ConfirmationID, "deny"); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	view, rpcErr := m.GetOperation(owner.SessionID, "op-denied")
	if rpcErr != nil || view.Status != "failed" || protocol.ErrorKind(view.Error) != "USER_DENIED" {
		t.Fatalf("denied operation = (%+v, %v)", view, rpcErr)
	}
	_, rpcErr = m.GetOperation(other.SessionID, "op-denied")
	assertRPCError(t, rpcErr, protocol.CodeInvalidState, "INVALID_REQUEST")
}

func TestCapabilityConfirmationIsTrustedFailClosedAndDeduplicated(t *testing.T) {
	m := newTestManager(t, time.Minute)
	session := m.OpenSession("capabilities", "test")
	if len(session.Capabilities) != 0 {
		t.Fatalf("new session capabilities = %v, want none", session.Capabilities)
	}

	assertRPCError(t, m.RequireCapability(session.SessionID, "unsafe.cdp"), protocol.CodeCapabilityDenied, "CAPABILITY_REQUIRED")
	confirmation, rpcErr := m.CreateCapabilityConfirmation(session.SessionID, "browser-1", []string{"unsafe.cdp", "clipboard.read", "unsafe.cdp"}, time.Minute)
	if rpcErr != nil {
		t.Fatalf("CreateCapabilityConfirmation() error = %v", rpcErr)
	}
	if got, want := confirmation.Capabilities, []string{"clipboard.read", "unsafe.cdp"}; !equalStrings(got, want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
	assertRPCError(t, m.RequireCapability(session.SessionID, "unsafe.cdp"), protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED")

	duplicate, rpcErr := m.CreateCapabilityConfirmation(session.SessionID, "browser-1", []string{"clipboard.read", "unsafe.cdp"}, time.Minute)
	if rpcErr != nil {
		t.Fatalf("duplicate CreateCapabilityConfirmation() error = %v", rpcErr)
	}
	if duplicate.ConfirmationID != confirmation.ConfirmationID {
		t.Fatalf("pending duplicate confirmation ID = %q, want %q", duplicate.ConfirmationID, confirmation.ConfirmationID)
	}

	resolved, rpcErr := m.ResolveConfirmation(confirmation.ConfirmationID, "approve")
	if rpcErr != nil || resolved.Status != "approved" {
		t.Fatalf("ResolveConfirmation() = (%+v, %v)", resolved, rpcErr)
	}
	if rpcErr := m.RequireCapability(session.SessionID, "unsafe.cdp"); rpcErr != nil {
		t.Fatalf("RequireCapability(after approval) error = %v", rpcErr)
	}
	updated, rpcErr := m.GetSession(session.SessionID)
	if rpcErr != nil || !updated.Capabilities["unsafe.cdp"] || !updated.Capabilities["clipboard.read"] {
		t.Fatalf("approved session = (%+v, %v)", updated, rpcErr)
	}

	if _, rpcErr := m.CreateCapabilityConfirmation(session.SessionID, "browser-1", []string{"unknown.capability"}, time.Minute); rpcErr == nil {
		t.Fatal("unknown capability confirmation unexpectedly succeeded")
	} else {
		assertRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "CAPABILITY_REQUIRED")
	}
}

func TestActionConfirmationExposesMetadataAndHashNotRawRequest(t *testing.T) {
	m := newTestManager(t, time.Minute)
	session := m.OpenSession("action-confirmation", "test")
	raw := json.RawMessage(`{"title":"Submit payment","summary":"Charge the saved card","origin":"https://shop.test","secret":"must-not-leak"}`)
	confirmation, rpcErr := m.CreateActionConfirmation(session.SessionID, "tab-1", "browser-1", raw, time.Minute)
	if rpcErr != nil {
		t.Fatalf("CreateActionConfirmation() error = %v", rpcErr)
	}
	if confirmation.Title != "Submit payment" || confirmation.Summary != "Charge the saved card" || confirmation.Origin != "https://shop.test" {
		t.Fatalf("confirmation metadata = %+v", confirmation)
	}
	encoded, err := json.Marshal(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("must-not-leak")) || bytes.Contains(encoded, []byte(`"secret"`)) {
		t.Fatalf("confirmation leaked raw request: %s", encoded)
	}
	if confirmation.RequestHash == "" {
		t.Fatal("confirmation request hash is empty")
	}

	if _, rpcErr := m.ResolveConfirmation(confirmation.ConfirmationID, "invalid"); rpcErr == nil {
		t.Fatal("invalid confirmation decision unexpectedly succeeded")
	}
	denied, rpcErr := m.ResolveConfirmation(confirmation.ConfirmationID, "deny")
	if rpcErr != nil || denied.Status != "denied" {
		t.Fatalf("denied confirmation = (%+v, %v)", denied, rpcErr)
	}
}

func TestActionConfirmationBindsApprovalToExactRequestAndConsumes(t *testing.T) {
	m := newTestManager(t, time.Minute)
	session := m.OpenSession("action-binding", "test")
	requestHash := "ab" + strings.Repeat("0", 62)
	raw := json.RawMessage(fmt.Sprintf(`{"title":"Confirm action","summary":"Submit form","origin":"https://shop.test","requestHash":%q}`, requestHash))
	confirmation, rpcErr := m.CreateActionConfirmation(session.SessionID, "tab-1", "browser-1", raw, time.Minute)
	if rpcErr != nil {
		t.Fatalf("CreateActionConfirmation() error = %v", rpcErr)
	}
	if confirmation.RequestHash != requestHash {
		t.Fatalf("request hash = %q, want %q", confirmation.RequestHash, requestHash)
	}

	_, rpcErr = m.AuthorizeActionConfirmation(confirmation.ConfirmationID, session.SessionID, "tab-1", "browser-1", requestHash)
	assertRPCError(t, rpcErr, protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED")
	if _, rpcErr := m.ResolveConfirmation(confirmation.ConfirmationID, "approve"); rpcErr != nil {
		t.Fatalf("ResolveConfirmation() error = %v", rpcErr)
	}
	if _, rpcErr := m.AuthorizeActionConfirmation(confirmation.ConfirmationID, session.SessionID, "tab-2", "browser-1", requestHash); rpcErr == nil {
		t.Fatal("confirmation authorized a different tab")
	} else {
		assertRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")
	}
	if _, rpcErr := m.AuthorizeActionConfirmation(confirmation.ConfirmationID, session.SessionID, "tab-1", "browser-1", strings.Repeat("f", 64)); rpcErr == nil {
		t.Fatal("confirmation authorized a different request hash")
	} else {
		assertRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")
	}
	if _, rpcErr := m.AuthorizeActionConfirmation(confirmation.ConfirmationID, session.SessionID, "tab-1", "browser-1", requestHash); rpcErr != nil {
		t.Fatalf("AuthorizeActionConfirmation() error = %v", rpcErr)
	}
	if rpcErr := m.ConsumeActionConfirmation(confirmation.ConfirmationID); rpcErr != nil {
		t.Fatalf("ConsumeActionConfirmation() error = %v", rpcErr)
	}
	consumed, rpcErr := m.GetConfirmation(confirmation.ConfirmationID)
	if rpcErr != nil || consumed.Status != "consumed" {
		t.Fatalf("consumed confirmation = (%+v, %v)", consumed, rpcErr)
	}
}

func TestClosingSessionCancelsPendingConfirmations(t *testing.T) {
	m := newTestManager(t, time.Minute)
	session := m.OpenSession("confirmation-close", "test")
	confirmation, rpcErr := m.CreateCapabilityConfirmation(session.SessionID, "browser-1", []string{"clipboard.read"}, time.Minute)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	resolved := make(chan Confirmation, 1)
	m.SetConfirmationResolvedHook(func(value Confirmation) { resolved <- value })
	if _, rpcErr := m.CloseSession(session.SessionID, "test close"); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	stored, rpcErr := m.GetConfirmation(confirmation.ConfirmationID)
	if rpcErr != nil || stored.Status != "cancelled" || stored.ResolvedAt == nil {
		t.Fatalf("cancelled confirmation = (%+v, %v)", stored, rpcErr)
	}
	select {
	case value := <-resolved:
		if value.ConfirmationID != confirmation.ConfirmationID || value.Status != "cancelled" {
			t.Fatalf("resolved hook = %+v", value)
		}
	case <-time.After(time.Second):
		t.Fatal("session close did not notify confirmation UI")
	}
}

func newTestManager(t *testing.T, leaseTTL time.Duration) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), leaseTTL, 256)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

func assertRPCError(t *testing.T, got *protocol.RPCError, code int, kind string) {
	t.Helper()
	if got == nil {
		t.Fatalf("RPC error = nil, want code %d kind %q", code, kind)
	}
	if got.Code != code || protocol.ErrorKind(got) != kind {
		t.Fatalf("RPC error = %+v (kind %q), want code %d kind %q", got, protocol.ErrorKind(got), code, kind)
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForQueueLength(t *testing.T, m *Manager, tabID string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.queuesMu.Lock()
		queue := m.tabQueues[tabID]
		m.queuesMu.Unlock()
		if queue != nil {
			queue.mu.Lock()
			got := len(queue.waiters)
			queue.mu.Unlock()
			if got == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d queued request(s) on %s", want, tabID)
}

func waitForWaiters(t *testing.T, q *tabQueue, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		got := len(q.waiters)
		q.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d FIFO waiter(s)", want)
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
