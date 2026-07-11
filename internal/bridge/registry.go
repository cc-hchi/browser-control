package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/cc-hchi/browser-control/internal/protocol"
	"github.com/cc-hchi/browser-control/internal/rpc"
)

type Hello struct {
	ProtocolVersion   string         `json:"protocolVersion"`
	BrowserInstanceID string         `json:"browserInstanceId"`
	ProfileName       string         `json:"profileName,omitempty"`
	ChromeVersion     string         `json:"chromeVersion,omitempty"`
	ExtensionVersion  string         `json:"extensionVersion,omitempty"`
	Capabilities      map[string]any `json:"capabilities,omitempty"`
}

type Instance struct {
	Hello
	ConnectedAt time.Time `json:"connectedAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	peer        *rpc.Peer
}

type Registry struct {
	mu        sync.RWMutex
	instances map[string]*Instance
	byPeer    map[*rpc.Peer]map[string]bool
	lastID    string
}

func NewRegistry() *Registry {
	return &Registry{instances: make(map[string]*Instance), byPeer: make(map[*rpc.Peer]map[string]bool)}
}

func (r *Registry) Register(peer *rpc.Peer, hello Hello) (Instance, *protocol.RPCError) {
	if hello.ProtocolVersion != protocol.Version {
		return Instance{}, protocol.NewError(protocol.CodeProtocolMismatch, "INVALID_REQUEST", "extension protocol version is incompatible", false, map[string]any{"expected": protocol.Version, "actual": hello.ProtocolVersion})
	}
	if hello.BrowserInstanceID == "" {
		return Instance{}, protocol.InvalidParams("browserInstanceId is required", nil)
	}
	now := time.Now().UTC()
	instance := &Instance{Hello: hello, ConnectedAt: now, LastSeenAt: now, peer: peer}
	r.mu.Lock()
	if old := r.instances[hello.BrowserInstanceID]; old != nil && old.peer != peer {
		_ = old.peer.Close()
	}
	r.instances[hello.BrowserInstanceID] = instance
	if r.byPeer[peer] == nil {
		r.byPeer[peer] = make(map[string]bool)
	}
	r.byPeer[peer][hello.BrowserInstanceID] = true
	r.lastID = hello.BrowserInstanceID
	r.mu.Unlock()
	return instance.public(), nil
}

func (r *Registry) Touch(peer *rpc.Peer) {
	r.mu.Lock()
	now := time.Now().UTC()
	for id := range r.byPeer[peer] {
		if instance := r.instances[id]; instance != nil && instance.peer == peer {
			instance.LastSeenAt = now
		}
	}
	r.mu.Unlock()
}

func (r *Registry) Disconnect(peer *rpc.Peer) []Instance {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := r.byPeer[peer]
	delete(r.byPeer, peer)
	disconnected := make([]Instance, 0, len(ids))
	for id := range ids {
		if instance := r.instances[id]; instance != nil && instance.peer == peer {
			disconnected = append(disconnected, instance.public())
			delete(r.instances, id)
		}
	}
	if r.lastID != "" {
		if _, exists := r.instances[r.lastID]; !exists {
			r.lastID = ""
			for id := range r.instances {
				r.lastID = id
				break
			}
		}
	}
	return disconnected
}

func (r *Registry) List() []Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Instance, 0, len(r.instances))
	for _, instance := range r.instances {
		result = append(result, instance.public())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].BrowserInstanceID < result[j].BrowserInstanceID })
	return result
}

// DefaultID returns the most recently registered live browser instance. It is
// intentionally only a convenience for the single-profile personal-tool case;
// callers may always provide an explicit browserInstanceId.
func (r *Registry) DefaultID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastID
}

// IDForPeer returns a deterministic browser instance owned by peer.
func (r *Registry) IDForPeer(peer *rpc.Peer) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.byPeer[peer]))
	for id := range r.byPeer[peer] {
		if instance := r.instances[id]; instance != nil && instance.peer == peer {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (r *Registry) PeerOwns(peer *rpc.Peer, browserInstanceID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	instance := r.instances[browserInstanceID]
	return instance != nil && instance.peer == peer
}

// Notify sends a daemon-owned event to the trusted extension connection. The
// public socket never exposes this direction, which keeps confirmation approval
// and administrative UI state off the AI-facing channel.
func (r *Registry) Notify(browserInstanceID, method string, params any) (string, error) {
	r.mu.RLock()
	if browserInstanceID == "" {
		browserInstanceID = r.lastID
	}
	instance := r.instances[browserInstanceID]
	r.mu.RUnlock()
	if instance == nil {
		return "", net.ErrClosed
	}
	if err := instance.peer.Notify(method, params); err != nil {
		return "", err
	}
	r.Touch(instance.peer)
	return browserInstanceID, nil
}

func (r *Registry) Call(ctx context.Context, browserInstanceID, method string, params json.RawMessage) (json.RawMessage, *protocol.RPCError) {
	r.mu.RLock()
	if browserInstanceID == "" {
		browserInstanceID = r.lastID
	}
	instance := r.instances[browserInstanceID]
	r.mu.RUnlock()
	if instance == nil {
		return nil, protocol.NewError(protocol.CodeBridgeUnavailable, "EXTENSION_DISCONNECTED", "no compatible Chrome extension is connected", true, map[string]any{"browserInstanceId": browserInstanceID})
	}
	var value any
	if len(params) > 0 {
		value = json.RawMessage(params)
	}
	result, rpcErr, err := instance.peer.Call(ctx, method, value)
	if err != nil {
		kind := "EXTENSION_DISCONNECTED"
		code := protocol.CodeBridgeUnavailable
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			kind = "TIMEOUT"
			code = protocol.CodeBridgeTimeout
		} else if !errors.Is(err, net.ErrClosed) {
			kind = "CHROME_DISCONNECTED"
		}
		return nil, protocol.NewError(code, kind, err.Error(), true, map[string]any{"browserInstanceId": browserInstanceID, "method": method})
	}
	r.Touch(instance.peer)
	return result, rpcErr
}

func (i *Instance) public() Instance {
	copy := *i
	copy.peer = nil
	return copy
}
