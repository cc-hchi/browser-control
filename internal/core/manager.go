package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cc-hchi/browser-control/internal/protocol"
)

type Session struct {
	SessionID    string          `json:"sessionId"`
	Name         string          `json:"name,omitempty"`
	ClientID     string          `json:"clientId,omitempty"`
	Capabilities map[string]bool `json:"capabilities"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
	ClosedAt     *time.Time      `json:"closedAt,omitempty"`
	Status       string          `json:"status"`
	OwnedTabs    map[string]bool `json:"ownedTabs,omitempty"`
}

type Lease struct {
	LeaseID           string    `json:"leaseId"`
	SessionID         string    `json:"sessionId"`
	TabID             string    `json:"tabId"`
	BrowserInstanceID string    `json:"browserInstanceId,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
	LastRenewed       time.Time `json:"lastRenewedAt"`
	ExpiresAt         time.Time `json:"expiresAt"`
	Revoked           bool      `json:"revoked,omitempty"`
	RevokeReason      string    `json:"revokeReason,omitempty"`
}

type Event struct {
	Seq           uint64          `json:"seq"`
	Time          time.Time       `json:"time"`
	Type          string          `json:"type"`
	SessionID     string          `json:"sessionId,omitempty"`
	TabID         string          `json:"tabId,omitempty"`
	DocumentEpoch *uint64         `json:"documentEpoch,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

type EventFilter struct {
	AfterSeq  uint64
	Types     map[string]bool
	SessionID string
	TabID     string
}

type Artifact struct {
	ArtifactID string    `json:"artifactId"`
	SessionID  string    `json:"sessionId,omitempty"`
	TabID      string    `json:"tabId,omitempty"`
	Kind       string    `json:"kind"`
	MIMEType   string    `json:"mimeType,omitempty"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	FileName   string    `json:"fileName,omitempty"`
	URI        string    `json:"uri"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	path       string
}

const MaxArtifactSize = 64 * 1024 * 1024

type operation struct {
	ID          string
	SessionID   string
	TabID       string
	Fingerprint string
	Status      string
	Result      json.RawMessage
	Err         *protocol.RPCError
	CreatedAt   time.Time
	CompletedAt time.Time
	done        chan struct{}
	cancel      context.CancelFunc
}

type OperationView struct {
	OperationID string             `json:"operationId"`
	SessionID   string             `json:"sessionId"`
	Status      string             `json:"status"`
	Result      json.RawMessage    `json:"result,omitempty"`
	Error       *protocol.RPCError `json:"error,omitempty"`
	CreatedAt   time.Time          `json:"createdAt"`
	CompletedAt *time.Time         `json:"completedAt,omitempty"`
}

type Confirmation struct {
	ConfirmationID    string     `json:"confirmationId"`
	OperationID       string     `json:"operationId,omitempty"`
	Kind              string     `json:"kind"`
	SessionID         string     `json:"sessionId"`
	SessionName       string     `json:"sessionName,omitempty"`
	ClientID          string     `json:"clientId,omitempty"`
	TabID             string     `json:"tabId,omitempty"`
	BrowserInstanceID string     `json:"browserInstanceId,omitempty"`
	RequestHash       string     `json:"requestHash"`
	Capabilities      []string   `json:"capabilities,omitempty"`
	Title             string     `json:"title,omitempty"`
	Summary           string     `json:"summary,omitempty"`
	Origin            string     `json:"origin,omitempty"`
	Status            string     `json:"status"`
	CreatedAt         time.Time  `json:"createdAt"`
	ExpiresAt         time.Time  `json:"expiresAt"`
	ResolvedAt        *time.Time `json:"resolvedAt,omitempty"`
}

type Manager struct {
	mu                       sync.Mutex
	sessions                 map[string]*Session
	leases                   map[string]*Lease
	operations               map[string]*operation
	events                   []Event
	nextEventSeq             uint64
	eventSignal              chan struct{}
	eventLimit               int
	leaseTTL                 time.Duration
	artifactRoot             string
	artifacts                map[string]*Artifact
	confirmations            map[string]*Confirmation
	leaseExpiredHook         func(Lease)
	confirmationResolvedHook func(Confirmation)
	queuesMu                 sync.Mutex
	tabQueues                map[string]*tabQueue
	closed                   chan struct{}
	closeOnce                sync.Once
}

func NewManager(artifactRoot string, leaseTTL time.Duration, eventLimit int) (*Manager, error) {
	if leaseTTL <= 0 {
		leaseTTL = 30 * time.Second
	}
	if eventLimit <= 0 {
		eventLimit = 1024
	}
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact root: %w", err)
	}
	if err := os.Chmod(artifactRoot, 0o700); err != nil {
		return nil, fmt.Errorf("secure artifact root: %w", err)
	}
	m := &Manager{
		sessions:      make(map[string]*Session),
		leases:        make(map[string]*Lease),
		operations:    make(map[string]*operation),
		eventSignal:   make(chan struct{}),
		eventLimit:    eventLimit,
		leaseTTL:      leaseTTL,
		artifactRoot:  artifactRoot,
		artifacts:     make(map[string]*Artifact),
		confirmations: make(map[string]*Confirmation),
		tabQueues:     make(map[string]*tabQueue),
		closed:        make(chan struct{}),
	}
	go m.janitor()
	return m, nil
}

func (m *Manager) SetLeaseExpiredHook(hook func(Lease)) {
	m.mu.Lock()
	m.leaseExpiredHook = hook
	m.mu.Unlock()
}

func (m *Manager) SetConfirmationResolvedHook(hook func(Confirmation)) {
	m.mu.Lock()
	m.confirmationResolvedHook = hook
	m.mu.Unlock()
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() { close(m.closed) })
}

func newID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + hex.EncodeToString(b)
}

func cloneSession(s *Session) Session {
	c := *s
	c.Capabilities = make(map[string]bool, len(s.Capabilities))
	for k, v := range s.Capabilities {
		c.Capabilities[k] = v
	}
	c.OwnedTabs = make(map[string]bool, len(s.OwnedTabs))
	for k, v := range s.OwnedTabs {
		c.OwnedTabs[k] = v
	}
	return c
}

func cloneLease(l *Lease) Lease {
	return *l
}

func (m *Manager) OpenSession(name, clientID string) Session {
	now := time.Now().UTC()
	s := &Session{
		SessionID:    newID("ses_"),
		Name:         name,
		ClientID:     clientID,
		Capabilities: make(map[string]bool),
		CreatedAt:    now,
		UpdatedAt:    now,
		Status:       "open",
		OwnedTabs:    make(map[string]bool),
	}
	m.mu.Lock()
	m.sessions[s.SessionID] = s
	m.emitLocked(Event{Type: "session.opened", SessionID: s.SessionID})
	result := cloneSession(s)
	m.mu.Unlock()
	return result
}

func (m *Manager) GetSession(id string) (Session, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, protocol.NewError(protocol.CodeSessionNotFound, "SESSION_NOT_FOUND", "session does not exist", false, map[string]any{"sessionId": id})
	}
	return cloneSession(s), nil
}

func (m *Manager) ListSessions() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, cloneSession(s))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func (m *Manager) CreateCapabilityConfirmation(sessionID, browserInstanceID string, requested []string, ttl time.Duration) (Confirmation, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, rpcErr := m.openSessionLocked(sessionID)
	if rpcErr != nil {
		return Confirmation{}, rpcErr
	}
	requested = uniqueSorted(requested)
	if len(requested) == 0 {
		return Confirmation{}, protocol.InvalidParams("at least one capability is required", nil)
	}
	for _, capability := range requested {
		if !knownCapability(capability) {
			return Confirmation{}, protocol.NewError(protocol.CodeCapabilityDenied, "CAPABILITY_REQUIRED", "unknown capability cannot be granted", false, map[string]any{"capability": capability})
		}
	}
	payload := protocol.MarshalResult(map[string]any{"sessionId": sessionID, "browserInstanceId": browserInstanceID, "capabilities": requested})
	return m.createConfirmationLocked("capability", sessionID, "", browserInstanceID, "", requested, payload, "", "Grant browser capabilities", strings.Join(requested, ", "), "", ttl), nil
}

func (m *Manager) CreateActionConfirmation(sessionID, tabID, browserInstanceID string, request json.RawMessage, ttl time.Duration) (Confirmation, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, rpcErr := m.openSessionLocked(sessionID); rpcErr != nil {
		return Confirmation{}, rpcErr
	}
	var metadata map[string]any
	_ = json.Unmarshal(request, &metadata)
	requestHash, _ := metadata["requestHash"].(string)
	operationID, _ := metadata["operationId"].(string)
	if !isSHA256(requestHash) {
		requestHash = ""
	}
	return m.createConfirmationLocked(
		"action", sessionID, tabID, browserInstanceID, operationID, nil, request, requestHash,
		limitedString(metadata, "title", 120), limitedString(metadata, "summary", 500), limitedString(metadata, "origin", 300), ttl,
	), nil
}

// AuthorizeActionConfirmation verifies that trusted UI approval is bound to the
// exact action preflight, browser instance, session, and tab. Approval is never
// accepted from the public client socket; this method only validates a decision
// already recorded through the trusted bridge handler.
func (m *Manager) AuthorizeActionConfirmation(id, sessionID, tabID, browserInstanceID, requestHash string) (Confirmation, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.confirmations[id]
	if !ok || c.Kind != "action" {
		return Confirmation{}, protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "a matching trusted action confirmation is required", false, map[string]any{"confirmationId": id})
	}
	if c.SessionID != sessionID || c.TabID != tabID || c.BrowserInstanceID != browserInstanceID || c.RequestHash != requestHash {
		return Confirmation{}, protocol.NewError(protocol.CodeCapabilityDenied, "PERMISSION_DENIED", "confirmation is not bound to this exact browser action", false, map[string]any{"confirmationId": id})
	}
	now := time.Now().UTC()
	if !c.ExpiresAt.After(now) {
		c.Status = "expired"
		c.ResolvedAt = &now
		m.finishAwaitingOperationLocked(c.OperationID, "failed", protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_EXPIRED", "confirmation expired before the action executed", false, map[string]any{"confirmationId": id, "operationId": c.OperationID}))
		resolved := cloneConfirmation(c)
		m.emitLocked(Event{Type: "confirmation.resolved", SessionID: c.SessionID, TabID: c.TabID, Payload: protocol.MarshalResult(c)})
		if hook := m.confirmationResolvedHook; hook != nil {
			go hook(resolved)
		}
		return Confirmation{}, protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "confirmation expired before the action executed", false, map[string]any{"confirmationId": id})
	}
	switch c.Status {
	case "approved", "consumed":
		return cloneConfirmation(c), nil
	case "denied":
		return Confirmation{}, protocol.NewError(protocol.CodeConfirmationNeeded, "USER_DENIED", "the user denied this browser action", false, map[string]any{"confirmationId": id})
	default:
		return Confirmation{}, protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "the action is waiting for trusted UI approval", true, map[string]any{"confirmationId": id, "expiresAt": c.ExpiresAt})
	}
}

func (m *Manager) ConsumeActionConfirmation(id string) *protocol.RPCError {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.confirmations[id]
	if !ok || c.Kind != "action" {
		return protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "action confirmation does not exist", false, map[string]any{"confirmationId": id})
	}
	if c.Status == "approved" {
		c.Status = "consumed"
	}
	if c.Status != "consumed" {
		return protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "action confirmation is not approved", false, map[string]any{"confirmationId": id, "status": c.Status})
	}
	return nil
}

func (m *Manager) ResolveConfirmation(id, decision string) (Confirmation, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.confirmations[id]
	if !ok {
		return Confirmation{}, protocol.NewError(protocol.CodeInvalidState, "INVALID_REQUEST", "confirmation does not exist", false, map[string]any{"confirmationId": id})
	}
	if c.Status != "pending" {
		return cloneConfirmation(c), nil
	}
	now := time.Now().UTC()
	if now.After(c.ExpiresAt) {
		c.Status = "expired"
		c.ResolvedAt = &now
		m.finishAwaitingOperationLocked(c.OperationID, "failed", protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_EXPIRED", "confirmation expired", false, map[string]any{"confirmationId": id, "operationId": c.OperationID}))
		resolved := cloneConfirmation(c)
		m.emitLocked(Event{Type: "confirmation.resolved", SessionID: c.SessionID, TabID: c.TabID, Payload: protocol.MarshalResult(c)})
		if hook := m.confirmationResolvedHook; hook != nil {
			go hook(resolved)
		}
		return Confirmation{}, protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "confirmation expired", false, map[string]any{"confirmationId": id})
	}
	switch decision {
	case "approve", "approved":
		c.Status = "approved"
		if c.Kind == "capability" {
			s, rpcErr := m.openSessionLocked(c.SessionID)
			if rpcErr != nil {
				return Confirmation{}, rpcErr
			}
			for _, capability := range c.Capabilities {
				s.Capabilities[capability] = true
			}
			s.UpdatedAt = now
		}
	case "deny", "denied":
		c.Status = "denied"
		m.finishAwaitingOperationLocked(c.OperationID, "failed", protocol.NewError(protocol.CodeConfirmationNeeded, "USER_DENIED", "the user denied this browser action", false, map[string]any{"confirmationId": id, "operationId": c.OperationID}))
	default:
		return Confirmation{}, protocol.InvalidParams("decision must be approve or deny", nil)
	}
	c.ResolvedAt = &now
	m.emitLocked(Event{Type: "confirmation.resolved", SessionID: c.SessionID, TabID: c.TabID, Payload: protocol.MarshalResult(c)})
	return cloneConfirmation(c), nil
}

func (m *Manager) GetConfirmation(id string) (Confirmation, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.confirmations[id]
	if !ok {
		return Confirmation{}, protocol.NewError(protocol.CodeInvalidState, "INVALID_REQUEST", "confirmation does not exist", false, map[string]any{"confirmationId": id})
	}
	return cloneConfirmation(c), nil
}

func (m *Manager) ListConfirmations(sessionID, status string) []Confirmation {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Confirmation, 0)
	for _, c := range m.confirmations {
		if sessionID != "" && c.SessionID != sessionID {
			continue
		}
		if status != "" && c.Status != status {
			continue
		}
		result = append(result, cloneConfirmation(c))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func (m *Manager) RequireCapability(sessionID, capability string) *protocol.RPCError {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, rpcErr := m.openSessionLocked(sessionID)
	if rpcErr != nil {
		return rpcErr
	}
	if !s.Capabilities[capability] {
		for _, confirmation := range m.confirmations {
			if confirmation.SessionID != sessionID || confirmation.Kind != "capability" || confirmation.Status != "pending" {
				continue
			}
			for _, requested := range confirmation.Capabilities {
				if requested == capability {
					return protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "capability is waiting for trusted UI approval", true, map[string]any{"sessionId": sessionID, "capability": capability, "confirmationId": confirmation.ConfirmationID})
				}
			}
		}
		return protocol.NewError(protocol.CodeCapabilityDenied, "CAPABILITY_REQUIRED", "session lacks required capability", false, map[string]any{"sessionId": sessionID, "capability": capability})
	}
	return nil
}

func (m *Manager) CloseSession(sessionID, reason string) ([]Lease, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, protocol.NewError(protocol.CodeSessionNotFound, "SESSION_NOT_FOUND", "session does not exist", false, map[string]any{"sessionId": sessionID})
	}
	if s.Status != "open" {
		return nil, nil
	}
	now := time.Now().UTC()
	s.Status = "closed"
	s.UpdatedAt = now
	s.ClosedAt = &now
	released := m.releaseSessionLeasesLocked(sessionID, reason)
	m.cancelOperationsLocked(sessionID, "", reason)
	for _, confirmation := range m.confirmations {
		if confirmation.SessionID != sessionID || confirmation.Status != "pending" {
			continue
		}
		confirmation.Status = "cancelled"
		confirmation.ResolvedAt = &now
		copy := cloneConfirmation(confirmation)
		m.emitLocked(Event{Type: "confirmation.resolved", SessionID: confirmation.SessionID, TabID: confirmation.TabID, Payload: protocol.MarshalResult(confirmation)})
		if hook := m.confirmationResolvedHook; hook != nil {
			go hook(copy)
		}
	}
	m.emitLocked(Event{Type: "session.closed", SessionID: sessionID, Payload: protocol.MarshalResult(map[string]any{"reason": reason})})
	return released, nil
}

func (m *Manager) StopAll(reason string) []Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := make([]Lease, 0, len(m.leases))
	now := time.Now().UTC()
	for _, s := range m.sessions {
		if s.Status == "open" {
			s.Status = "closed"
			s.UpdatedAt = now
			s.ClosedAt = &now
		}
	}
	for tabID, l := range m.leases {
		l.Revoked = true
		l.RevokeReason = reason
		all = append(all, cloneLease(l))
		delete(m.leases, tabID)
		m.cancelTabQueue(tabID, reason)
	}
	for _, confirmation := range m.confirmations {
		if confirmation.Status != "pending" {
			continue
		}
		confirmation.Status = "cancelled"
		confirmation.ResolvedAt = &now
		copy := cloneConfirmation(confirmation)
		m.emitLocked(Event{Type: "confirmation.resolved", SessionID: confirmation.SessionID, TabID: confirmation.TabID, Payload: protocol.MarshalResult(confirmation)})
		if hook := m.confirmationResolvedHook; hook != nil {
			go hook(copy)
		}
	}
	m.cancelOperationsLocked("", "", reason)
	m.emitLocked(Event{Type: "emergency.stop", Payload: protocol.MarshalResult(map[string]any{"reason": reason})})
	return all
}

func (m *Manager) ClaimTab(sessionID, tabID string, ttl time.Duration) (Lease, *protocol.RPCError) {
	if tabID == "" {
		return Lease{}, protocol.InvalidParams("tabId is required", nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, rpcErr := m.openSessionLocked(sessionID); rpcErr != nil {
		return Lease{}, rpcErr
	}
	now := time.Now().UTC()
	if current, ok := m.leases[tabID]; ok {
		if current.ExpiresAt.After(now) && !current.Revoked {
			if current.SessionID == sessionID {
				current.LastRenewed = now
				current.ExpiresAt = now.Add(m.effectiveTTL(ttl))
				return cloneLease(current), nil
			}
			return Lease{}, protocol.NewError(protocol.CodeLeaseConflict, "LEASE_CONFLICT", "tab is claimed by another session", true, map[string]any{"tabId": tabID, "expiresAt": current.ExpiresAt})
		}
		expired := cloneLease(current)
		delete(m.leases, tabID)
		m.cancelOperationsLocked(current.SessionID, tabID, "expired lease replaced")
		m.cancelTabQueue(tabID, "expired lease replaced")
		m.emitLocked(Event{Type: "lease.expired", SessionID: current.SessionID, TabID: tabID, Payload: protocol.MarshalResult(map[string]any{"leaseId": current.LeaseID})})
		if hook := m.leaseExpiredHook; hook != nil {
			go hook(expired)
		}
	}
	l := &Lease{
		LeaseID:     newID("lease_"),
		SessionID:   sessionID,
		TabID:       tabID,
		CreatedAt:   now,
		LastRenewed: now,
		ExpiresAt:   now.Add(m.effectiveTTL(ttl)),
	}
	m.leases[tabID] = l
	m.emitLocked(Event{Type: "tab.claimed", SessionID: sessionID, TabID: tabID, Payload: protocol.MarshalResult(map[string]any{"leaseId": l.LeaseID, "expiresAt": l.ExpiresAt})})
	return cloneLease(l), nil
}

func (m *Manager) RenewLease(sessionID, tabID, leaseID string, ttl time.Duration) (Lease, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, rpcErr := m.validLeaseLocked(sessionID, tabID, leaseID, true)
	if rpcErr != nil {
		return Lease{}, rpcErr
	}
	now := time.Now().UTC()
	l.LastRenewed = now
	l.ExpiresAt = now.Add(m.effectiveTTL(ttl))
	return cloneLease(l), nil
}

func (m *Manager) BindLeaseBrowser(sessionID, tabID, leaseID, browserInstanceID string) (Lease, *protocol.RPCError) {
	if browserInstanceID == "" {
		return Lease{}, protocol.InvalidParams("browserInstanceId is required", nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, rpcErr := m.validLeaseLocked(sessionID, tabID, leaseID, true)
	if rpcErr != nil {
		return Lease{}, rpcErr
	}
	if lease.BrowserInstanceID != "" && lease.BrowserInstanceID != browserInstanceID {
		return Lease{}, protocol.NewError(protocol.CodeLeaseConflict, "LEASE_CONFLICT", "lease belongs to a different Chrome instance", false, map[string]any{"tabId": tabID})
	}
	lease.BrowserInstanceID = browserInstanceID
	return cloneLease(lease), nil
}

func (m *Manager) ValidateLease(sessionID, tabID, leaseID string) *protocol.RPCError {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, rpcErr := m.validLeaseLocked(sessionID, tabID, leaseID, true)
	return rpcErr
}

func (m *Manager) ReleaseTab(sessionID, tabID, leaseID, reason string) (Lease, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, rpcErr := m.validLeaseLocked(sessionID, tabID, leaseID, false)
	if rpcErr != nil {
		return Lease{}, rpcErr
	}
	copy := cloneLease(l)
	delete(m.leases, tabID)
	m.cancelOperationsLocked(sessionID, tabID, reason)
	m.cancelTabQueue(tabID, reason)
	m.emitLocked(Event{Type: "tab.released", SessionID: sessionID, TabID: tabID, Payload: protocol.MarshalResult(map[string]any{"leaseId": leaseID, "reason": reason})})
	return copy, nil
}

func (m *Manager) MarkOwnedTab(sessionID, tabID string) *protocol.RPCError {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, rpcErr := m.openSessionLocked(sessionID)
	if rpcErr != nil {
		return rpcErr
	}
	s.OwnedTabs[tabID] = true
	s.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *Manager) CanCloseTab(sessionID, tabID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	return ok && s.OwnedTabs[tabID]
}

func (m *Manager) LeaseForTab(tabID string) (Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[tabID]
	if !ok || l.Revoked || !l.ExpiresAt.After(time.Now().UTC()) {
		return Lease{}, false
	}
	return cloneLease(l), true
}

// RunTab serializes browser work for one tab and makes queued and active work
// cancellable when the lease/session is revoked.
func (m *Manager) RunTab(ctx context.Context, tabID string, fn func(context.Context) (json.RawMessage, *protocol.RPCError)) (json.RawMessage, *protocol.RPCError) {
	if tabID == "" {
		return fn(ctx)
	}
	m.queuesMu.Lock()
	queue := m.tabQueues[tabID]
	if queue == nil {
		queue = &tabQueue{}
		m.tabQueues[tabID] = queue
	}
	m.queuesMu.Unlock()
	return queue.run(ctx, fn)
}

func (m *Manager) cancelTabQueue(tabID, reason string) {
	m.queuesMu.Lock()
	queue := m.tabQueues[tabID]
	m.queuesMu.Unlock()
	if queue != nil {
		queue.cancelAll(reason)
	}
}

func (m *Manager) cancelOperationsLocked(sessionID, tabID, reason string) {
	for _, op := range m.operations {
		if sessionID != "" && op.SessionID != sessionID {
			continue
		}
		if tabID != "" && op.TabID != tabID {
			continue
		}
		switch op.Status {
		case "running":
			if op.cancel != nil {
				op.cancel()
			}
		case "awaiting_confirmation":
			m.finishAwaitingOperationLocked(op.ID, "cancelled", protocol.NewError(protocol.CodeCancelled, "CANCELLED", reason, false, map[string]any{"operationId": op.ID}))
		}
	}
}

func (m *Manager) finishAwaitingOperationLocked(operationID, status string, rpcErr *protocol.RPCError) {
	op := m.operations[operationID]
	if op == nil || op.Status != "awaiting_confirmation" {
		return
	}
	op.Status = status
	op.Err = rpcErr
	op.CompletedAt = time.Now().UTC()
	m.emitLocked(Event{Type: "operation.updated", SessionID: op.SessionID, TabID: op.TabID, Payload: protocol.MarshalResult(map[string]any{"operationId": operationID, "status": status})})
}

func (m *Manager) ExecuteOperation(ctx context.Context, sessionID, tabID, operationID string, request any, fn func(context.Context) (json.RawMessage, *protocol.RPCError)) (json.RawMessage, *protocol.RPCError, bool) {
	if operationID == "" {
		return nil, protocol.InvalidParams("operationId is required", nil), false
	}
	fingerprintBytes, err := json.Marshal(request)
	if err != nil {
		return nil, protocol.InvalidParams("operation request cannot be encoded", nil), false
	}
	hash := sha256.Sum256(fingerprintBytes)
	fingerprint := hex.EncodeToString(hash[:])

	m.mu.Lock()
	if _, rpcErr := m.openSessionLocked(sessionID); rpcErr != nil {
		m.mu.Unlock()
		return nil, rpcErr, false
	}
	if existing, ok := m.operations[operationID]; ok {
		if existing.SessionID != sessionID || existing.Fingerprint != fingerprint {
			m.mu.Unlock()
			return nil, protocol.NewError(protocol.CodeOperationConflict, "INVALID_REQUEST", "operationId was already used for a different request", false, map[string]any{"operationId": operationID}), false
		}
		if existing.Status == "awaiting_confirmation" {
			delete(m.operations, operationID)
		} else if existing.Status != "running" {
			result, rpcErr := cloneRaw(existing.Result), existing.Err
			m.mu.Unlock()
			return result, rpcErr, true
		} else {
			done := existing.done
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, protocol.NewError(protocol.CodeCancelled, "CANCELLED", ctx.Err().Error(), true, map[string]any{"operationId": operationID}), true
			case <-done:
				m.mu.Lock()
				result, rpcErr := cloneRaw(existing.Result), existing.Err
				m.mu.Unlock()
				return result, rpcErr, true
			}
		}
	}
	opCtx, cancel := context.WithCancel(ctx)
	op := &operation{ID: operationID, SessionID: sessionID, TabID: tabID, Fingerprint: fingerprint, Status: "running", CreatedAt: time.Now().UTC(), done: make(chan struct{}), cancel: cancel}
	m.operations[operationID] = op
	m.emitLocked(Event{Type: "operation.updated", SessionID: sessionID, Payload: protocol.MarshalResult(map[string]any{"operationId": operationID, "status": "running"})})
	m.mu.Unlock()

	var result json.RawMessage
	var rpcErr *protocol.RPCError
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				rpcErr = protocol.Internal(fmt.Errorf("operation panic: %v", recovered))
			}
		}()
		result, rpcErr = fn(opCtx)
	}()
	cancel()

	m.mu.Lock()
	op.Result = cloneRaw(result)
	op.Err = rpcErr
	op.CompletedAt = time.Now().UTC()
	if protocol.ErrorKind(rpcErr) == "CONFIRMATION_REQUIRED" {
		op.Status = "awaiting_confirmation"
	} else if protocol.ErrorKind(rpcErr) == "CANCELLED" {
		op.Status = "cancelled"
	} else if rpcErr != nil {
		op.Status = "failed"
	} else {
		op.Status = "completed"
	}
	close(op.done)
	m.emitLocked(Event{Type: "operation.updated", SessionID: sessionID, Payload: protocol.MarshalResult(map[string]any{"operationId": operationID, "status": op.Status})})
	m.mu.Unlock()
	return result, rpcErr, false
}

func (m *Manager) CancelOperation(sessionID, operationID string) (OperationView, *protocol.RPCError) {
	m.mu.Lock()
	op, ok := m.operations[operationID]
	if !ok || op.SessionID != sessionID {
		m.mu.Unlock()
		return OperationView{}, protocol.NewError(protocol.CodeInvalidState, "INVALID_REQUEST", "operation does not exist", false, map[string]any{"operationId": operationID})
	}
	if op.Status == "awaiting_confirmation" {
		m.finishAwaitingOperationLocked(operationID, "cancelled", protocol.NewError(protocol.CodeCancelled, "CANCELLED", "operation cancelled while awaiting confirmation", false, map[string]any{"operationId": operationID}))
		now := time.Now().UTC()
		for _, confirmation := range m.confirmations {
			if confirmation.OperationID != operationID || confirmation.SessionID != sessionID || confirmation.Status != "pending" {
				continue
			}
			confirmation.Status = "cancelled"
			confirmation.ResolvedAt = &now
			copy := cloneConfirmation(confirmation)
			m.emitLocked(Event{Type: "confirmation.resolved", SessionID: confirmation.SessionID, TabID: confirmation.TabID, Payload: protocol.MarshalResult(confirmation)})
			if hook := m.confirmationResolvedHook; hook != nil {
				go hook(copy)
			}
		}
		view := operationView(op)
		m.mu.Unlock()
		return view, nil
	}
	if op.Status != "running" {
		view := operationView(op)
		m.mu.Unlock()
		return view, nil
	}
	cancel := op.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return m.GetOperation(sessionID, operationID)
}

func (m *Manager) RevokeTab(tabID, reason string) (Lease, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[tabID]
	if !ok {
		return Lease{}, protocol.NewError(protocol.CodeLeaseRequired, "LEASE_REQUIRED", "tab is not claimed", true, map[string]any{"tabId": tabID})
	}
	l.Revoked = true
	l.RevokeReason = reason
	copy := cloneLease(l)
	delete(m.leases, tabID)
	m.cancelOperationsLocked(l.SessionID, tabID, reason)
	m.cancelTabQueue(tabID, reason)
	m.emitLocked(Event{Type: "lease.revoked", SessionID: l.SessionID, TabID: tabID, Payload: protocol.MarshalResult(map[string]any{"leaseId": l.LeaseID, "reason": reason})})
	return copy, nil
}

func (m *Manager) RevokeBrowser(browserInstanceID, reason string) []Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	released := make([]Lease, 0)
	now := time.Now().UTC()
	for tabID, lease := range m.leases {
		if lease.BrowserInstanceID != browserInstanceID {
			continue
		}
		lease.Revoked = true
		lease.RevokeReason = reason
		released = append(released, cloneLease(lease))
		delete(m.leases, tabID)
		m.cancelOperationsLocked(lease.SessionID, tabID, reason)
		m.cancelTabQueue(tabID, reason)
		m.emitLocked(Event{Type: "lease.revoked", SessionID: lease.SessionID, TabID: tabID, Payload: protocol.MarshalResult(map[string]any{"leaseId": lease.LeaseID, "reason": reason})})
	}
	for _, confirmation := range m.confirmations {
		if confirmation.BrowserInstanceID != browserInstanceID || confirmation.Status != "pending" {
			continue
		}
		confirmation.Status = "cancelled"
		confirmation.ResolvedAt = &now
		m.emitLocked(Event{Type: "confirmation.resolved", SessionID: confirmation.SessionID, TabID: confirmation.TabID, Payload: protocol.MarshalResult(confirmation)})
	}
	return released
}

func (m *Manager) GetOperation(sessionID, operationID string) (OperationView, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[operationID]
	if !ok || op.SessionID != sessionID {
		return OperationView{}, protocol.NewError(protocol.CodeInvalidState, "INVALID_REQUEST", "operation does not exist", false, map[string]any{"operationId": operationID})
	}
	return operationView(op), nil
}

func (m *Manager) WaitOperation(ctx context.Context, sessionID, operationID string) (OperationView, *protocol.RPCError) {
	m.mu.Lock()
	op, ok := m.operations[operationID]
	if !ok || op.SessionID != sessionID {
		m.mu.Unlock()
		return OperationView{}, protocol.NewError(protocol.CodeInvalidState, "INVALID_REQUEST", "operation does not exist", false, map[string]any{"operationId": operationID})
	}
	done := op.done
	status := op.Status
	m.mu.Unlock()
	if status == "running" {
		select {
		case <-ctx.Done():
			return OperationView{}, protocol.NewError(protocol.CodeBridgeTimeout, "TIMEOUT", ctx.Err().Error(), true, map[string]any{"operationId": operationID})
		case <-done:
		}
	}
	return m.GetOperation(sessionID, operationID)
}

func operationView(op *operation) OperationView {
	v := OperationView{OperationID: op.ID, SessionID: op.SessionID, Status: op.Status, Result: cloneRaw(op.Result), Error: op.Err, CreatedAt: op.CreatedAt}
	if !op.CompletedAt.IsZero() {
		completed := op.CompletedAt
		v.CompletedAt = &completed
	}
	return v
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func (m *Manager) EmitEvent(event Event) Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.emitLocked(event)
}

func (m *Manager) ReplayEvents(filter EventFilter, limit int) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.matchEventsLocked(filter, limit)
}

func (m *Manager) NextEvent(ctx context.Context, filter EventFilter) (Event, *protocol.RPCError) {
	for {
		m.mu.Lock()
		matched := m.matchEventsLocked(filter, 1)
		if len(matched) > 0 {
			m.mu.Unlock()
			return matched[0], nil
		}
		signal := m.eventSignal
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return Event{}, protocol.NewError(protocol.CodeBridgeTimeout, "TIMEOUT", ctx.Err().Error(), true, map[string]any{"afterSeq": filter.AfterSeq})
		case <-signal:
		}
	}
}

func (m *Manager) StoreArtifact(sessionID, tabID, kind, mimeType, fileName string, data []byte, ttl time.Duration) (Artifact, *protocol.RPCError) {
	if sessionID != "" {
		m.mu.Lock()
		_, rpcErr := m.openSessionLocked(sessionID)
		m.mu.Unlock()
		if rpcErr != nil {
			return Artifact{}, rpcErr
		}
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if len(data) > MaxArtifactSize {
		return Artifact{}, protocol.NewError(protocol.CodeUnsupported, "UNSUPPORTED", "artifact exceeds the local size limit", false, map[string]any{"size": len(data), "limit": MaxArtifactSize})
	}
	id := newID("art_")
	cleanName := filepath.Base(fileName)
	if cleanName == "." || cleanName == string(filepath.Separator) || cleanName == "" {
		cleanName = id + ".bin"
	}
	path := filepath.Join(m.artifactRoot, id+"-"+cleanName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Artifact{}, protocol.Internal(fmt.Errorf("write artifact: %w", err))
	}
	hash := sha256.Sum256(data)
	now := time.Now().UTC()
	a := &Artifact{
		ArtifactID: id, SessionID: sessionID, TabID: tabID, Kind: kind, MIMEType: mimeType,
		Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:]), FileName: cleanName,
		URI: "browser-artifact://" + id, CreatedAt: now, ExpiresAt: now.Add(ttl), path: path,
	}
	m.mu.Lock()
	if sessionID != "" {
		if _, rpcErr := m.openSessionLocked(sessionID); rpcErr != nil {
			m.mu.Unlock()
			_ = os.Remove(path)
			return Artifact{}, rpcErr
		}
	}
	m.artifacts[id] = a
	m.mu.Unlock()
	return *a, nil
}

func (m *Manager) StoreArtifactBase64(sessionID, tabID, kind, mimeType, fileName, encoded string, ttl time.Duration) (Artifact, *protocol.RPCError) {
	if len(encoded) > base64.StdEncoding.EncodedLen(MaxArtifactSize) {
		return Artifact{}, protocol.NewError(protocol.CodeUnsupported, "UNSUPPORTED", "artifact exceeds the local size limit", false, map[string]any{"limit": MaxArtifactSize})
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Artifact{}, protocol.InvalidParams("artifact data is not valid base64", nil)
	}
	return m.StoreArtifact(sessionID, tabID, kind, mimeType, fileName, data, ttl)
}

func (m *Manager) GetArtifact(id string) (Artifact, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.artifacts[id]
	if !ok || time.Now().UTC().After(a.ExpiresAt) {
		return Artifact{}, protocol.NewError(protocol.CodeArtifactNotFound, "ARTIFACT_EXPIRED", "artifact does not exist or has expired", false, map[string]any{"artifactId": id})
	}
	return *a, nil
}

func (m *Manager) ArtifactLocalPath(id string) (string, *protocol.RPCError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.artifacts[id]
	if !ok || time.Now().UTC().After(a.ExpiresAt) {
		return "", protocol.NewError(protocol.CodeArtifactNotFound, "ARTIFACT_EXPIRED", "artifact does not exist or has expired", false, map[string]any{"artifactId": id})
	}
	return a.path, nil
}

func (m *Manager) ReadArtifactChunk(id string, offset, length int64) ([]byte, Artifact, *protocol.RPCError) {
	a, rpcErr := m.GetArtifact(id)
	if rpcErr != nil {
		return nil, Artifact{}, rpcErr
	}
	if offset < 0 || length < 0 {
		return nil, Artifact{}, protocol.InvalidParams("offset and length must be non-negative", nil)
	}
	file, err := os.Open(a.path)
	if err != nil {
		return nil, Artifact{}, protocol.Internal(fmt.Errorf("open artifact: %w", err))
	}
	defer file.Close()
	if offset > a.Size {
		offset = a.Size
	}
	if length == 0 || length > 512*1024 {
		length = 512 * 1024
	}
	remaining := a.Size - offset
	if length > remaining {
		length = remaining
	}
	data := make([]byte, length)
	if length > 0 {
		if _, err := file.ReadAt(data, offset); err != nil {
			return nil, Artifact{}, protocol.Internal(fmt.Errorf("read artifact: %w", err))
		}
	}
	return data, a, nil
}

func (m *Manager) DeleteArtifact(id string) *protocol.RPCError {
	m.mu.Lock()
	a, ok := m.artifacts[id]
	if ok {
		delete(m.artifacts, id)
	}
	m.mu.Unlock()
	if !ok {
		return protocol.NewError(protocol.CodeArtifactNotFound, "ARTIFACT_EXPIRED", "artifact does not exist or has expired", false, map[string]any{"artifactId": id})
	}
	if err := os.Remove(a.path); err != nil && !os.IsNotExist(err) {
		return protocol.Internal(fmt.Errorf("delete artifact: %w", err))
	}
	return nil
}

func (m *Manager) Stats() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	openSessions := 0
	for _, s := range m.sessions {
		if s.Status == "open" {
			openSessions++
		}
	}
	return map[string]any{
		"sessions":       len(m.sessions),
		"openSessions":   openSessions,
		"leases":         len(m.leases),
		"operations":     len(m.operations),
		"events":         len(m.events),
		"artifacts":      len(m.artifacts),
		"confirmations":  len(m.confirmations),
		"lastEventSeq":   m.nextEventSeq,
		"artifactRoot":   m.artifactRoot,
		"defaultLeaseMs": m.leaseTTL.Milliseconds(),
	}
}

func (m *Manager) createConfirmationLocked(kind, sessionID, tabID, browserInstanceID, operationID string, capabilities []string, request json.RawMessage, boundHash, title, summary, origin string, ttl time.Duration) Confirmation {
	if ttl <= 0 || ttl > 10*time.Minute {
		ttl = 2 * time.Minute
	}
	hash := sha256.Sum256(request)
	requestHash := hex.EncodeToString(hash[:])
	if isSHA256(boundHash) {
		requestHash = boundHash
	}
	for _, existing := range m.confirmations {
		if existing.Status == "pending" && existing.Kind == kind && existing.SessionID == sessionID && existing.TabID == tabID && existing.BrowserInstanceID == browserInstanceID && existing.OperationID == operationID && existing.RequestHash == requestHash {
			return cloneConfirmation(existing)
		}
	}
	now := time.Now().UTC()
	c := &Confirmation{
		ConfirmationID: newID("conf_"), Kind: kind, SessionID: sessionID, TabID: tabID,
		BrowserInstanceID: browserInstanceID, OperationID: operationID, RequestHash: requestHash,
		Capabilities: append([]string(nil), capabilities...), Title: title, Summary: summary,
		Origin: origin, Status: "pending", CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if session := m.sessions[sessionID]; session != nil {
		c.SessionName = session.Name
		c.ClientID = session.ClientID
	}
	m.confirmations[c.ConfirmationID] = c
	m.emitLocked(Event{Type: "confirmation.requested", SessionID: sessionID, TabID: tabID, Payload: protocol.MarshalResult(c)})
	return cloneConfirmation(c)
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func cloneConfirmation(c *Confirmation) Confirmation {
	copy := *c
	copy.Capabilities = append([]string(nil), c.Capabilities...)
	return copy
}

func limitedString(values map[string]any, key string, limit int) string {
	value, _ := values[key].(string)
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func uniqueSorted(values []string) []string {
	set := make(map[string]bool)
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func knownCapability(value string) bool {
	switch value {
	case "history.read", "clipboard.read", "clipboard.write", "files.upload", "files.download", "secureInput", "artifact.localPath", "unsafe.evaluate", "unsafe.cdp":
		return true
	default:
		return false
	}
}

func (m *Manager) openSessionLocked(id string) (*Session, *protocol.RPCError) {
	s, ok := m.sessions[id]
	if !ok {
		return nil, protocol.NewError(protocol.CodeSessionNotFound, "SESSION_NOT_FOUND", "session does not exist", false, map[string]any{"sessionId": id})
	}
	if s.Status != "open" {
		return nil, protocol.NewError(protocol.CodeSessionStopped, "SESSION_CLOSED", "session is closed", false, map[string]any{"sessionId": id})
	}
	return s, nil
}

func (m *Manager) validLeaseLocked(sessionID, tabID, leaseID string, requireOpenSession bool) (*Lease, *protocol.RPCError) {
	if requireOpenSession {
		if _, rpcErr := m.openSessionLocked(sessionID); rpcErr != nil {
			return nil, rpcErr
		}
	}
	l, ok := m.leases[tabID]
	if !ok {
		return nil, protocol.NewError(protocol.CodeLeaseRequired, "LEASE_REQUIRED", "tab is not claimed", true, map[string]any{"sessionId": sessionID, "tabId": tabID})
	}
	if !l.ExpiresAt.After(time.Now().UTC()) {
		expired := cloneLease(l)
		delete(m.leases, tabID)
		m.cancelOperationsLocked(l.SessionID, tabID, "lease expired")
		m.cancelTabQueue(tabID, "lease expired")
		m.emitLocked(Event{Type: "lease.expired", SessionID: l.SessionID, TabID: tabID, Payload: protocol.MarshalResult(map[string]any{"leaseId": l.LeaseID})})
		if hook := m.leaseExpiredHook; hook != nil {
			go hook(expired)
		}
		return nil, protocol.NewError(protocol.CodeLeaseExpired, "LEASE_EXPIRED", "tab lease expired", true, map[string]any{"sessionId": sessionID, "tabId": tabID, "leaseId": leaseID})
	}
	if l.Revoked {
		return nil, protocol.NewError(protocol.CodeLeaseExpired, "LEASE_REVOKED", "tab lease was revoked", false, map[string]any{"sessionId": sessionID, "tabId": tabID, "leaseId": leaseID, "reason": l.RevokeReason})
	}
	if l.SessionID != sessionID || l.LeaseID != leaseID {
		return nil, protocol.NewError(protocol.CodeLeaseConflict, "LEASE_CONFLICT", "lease does not belong to this session", false, map[string]any{"sessionId": sessionID, "tabId": tabID, "leaseId": leaseID})
	}
	return l, nil
}

func (m *Manager) effectiveTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return m.leaseTTL
	}
	if ttl < time.Second {
		return time.Second
	}
	if ttl > m.leaseTTL {
		return m.leaseTTL
	}
	return ttl
}

func (m *Manager) releaseSessionLeasesLocked(sessionID, reason string) []Lease {
	released := make([]Lease, 0)
	for tabID, l := range m.leases {
		if l.SessionID == sessionID {
			l.Revoked = true
			l.RevokeReason = reason
			released = append(released, cloneLease(l))
			delete(m.leases, tabID)
			m.cancelOperationsLocked(sessionID, tabID, reason)
			m.cancelTabQueue(tabID, reason)
			m.emitLocked(Event{Type: "tab.released", SessionID: sessionID, TabID: tabID, Payload: protocol.MarshalResult(map[string]any{"leaseId": l.LeaseID, "reason": reason})})
		}
	}
	return released
}

func (m *Manager) emitLocked(event Event) Event {
	m.nextEventSeq++
	event.Seq = m.nextEventSeq
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	event.Payload = cloneRaw(event.Payload)
	m.events = append(m.events, event)
	if len(m.events) > m.eventLimit {
		m.events = append([]Event(nil), m.events[len(m.events)-m.eventLimit:]...)
	}
	close(m.eventSignal)
	m.eventSignal = make(chan struct{})
	return event
}

func (m *Manager) matchEventsLocked(filter EventFilter, limit int) []Event {
	if limit <= 0 || limit > m.eventLimit {
		limit = m.eventLimit
	}
	result := make([]Event, 0, limit)
	for _, event := range m.events {
		if event.Seq <= filter.AfterSeq {
			continue
		}
		if len(filter.Types) > 0 && !filter.Types[event.Type] {
			continue
		}
		if filter.SessionID != "" && event.SessionID != filter.SessionID {
			continue
		}
		if filter.TabID != "" && event.TabID != filter.TabID {
			continue
		}
		event.Payload = cloneRaw(event.Payload)
		result = append(result, event)
		if len(result) == limit {
			break
		}
	}
	return result
}

func (m *Manager) janitor() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.closed:
			return
		case now := <-ticker.C:
			var expired []Lease
			var expiredConfirmations []Confirmation
			var hook func(Lease)
			var confirmationHook func(Confirmation)
			m.mu.Lock()
			for tabID, lease := range m.leases {
				if now.After(lease.ExpiresAt) {
					expired = append(expired, cloneLease(lease))
					delete(m.leases, tabID)
					m.cancelOperationsLocked(lease.SessionID, tabID, "lease expired")
					m.cancelTabQueue(tabID, "lease expired")
					m.emitLocked(Event{Type: "lease.expired", SessionID: lease.SessionID, TabID: tabID, Payload: protocol.MarshalResult(map[string]any{"leaseId": lease.LeaseID})})
				}
			}
			for id, op := range m.operations {
				if op.Status != "running" && now.Sub(op.CompletedAt) > time.Hour {
					delete(m.operations, id)
				}
			}
			for id, artifact := range m.artifacts {
				if now.After(artifact.ExpiresAt) {
					delete(m.artifacts, id)
					_ = os.Remove(artifact.path)
				}
			}
			for _, confirmation := range m.confirmations {
				if confirmation.Status == "pending" && now.After(confirmation.ExpiresAt) {
					resolved := now.UTC()
					confirmation.Status = "expired"
					confirmation.ResolvedAt = &resolved
					m.finishAwaitingOperationLocked(confirmation.OperationID, "failed", protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_EXPIRED", "confirmation expired", false, map[string]any{"confirmationId": confirmation.ConfirmationID, "operationId": confirmation.OperationID}))
					copy := cloneConfirmation(confirmation)
					expiredConfirmations = append(expiredConfirmations, copy)
					m.emitLocked(Event{Type: "confirmation.resolved", SessionID: confirmation.SessionID, TabID: confirmation.TabID, Payload: protocol.MarshalResult(confirmation)})
				}
			}
			hook = m.leaseExpiredHook
			confirmationHook = m.confirmationResolvedHook
			m.mu.Unlock()
			if hook != nil {
				for _, lease := range expired {
					go hook(lease)
				}
			}
			if confirmationHook != nil {
				for _, confirmation := range expiredConfirmations {
					go confirmationHook(confirmation)
				}
			}
		}
	}
}

func DecodeStringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result[value] = true
		}
	}
	return result
}
