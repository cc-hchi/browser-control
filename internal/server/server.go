package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cc-hchi/browser-control/internal/bridge"
	"github.com/cc-hchi/browser-control/internal/bridgeauth"
	"github.com/cc-hchi/browser-control/internal/config"
	"github.com/cc-hchi/browser-control/internal/core"
	"github.com/cc-hchi/browser-control/internal/protocol"
	"github.com/cc-hchi/browser-control/internal/rpc"
)

type Server struct {
	cfg            config.Config
	core           *core.Manager
	bridges        *bridge.Registry
	startedAt      time.Time
	publicListener net.Listener
	bridgeListener net.Listener
	clients        atomic.Int64
	bridgeClients  atomic.Int64
	bridgeToken    []byte
	bridgeAuthMu   sync.RWMutex
	bridgeAuth     map[*rpc.Peer]struct{}
	closed         chan struct{}
	closeOnce      sync.Once
	wg             sync.WaitGroup
}

func New(cfg config.Config) (*Server, error) {
	var err error
	cfg, err = cfg.Normalized()
	if err != nil {
		return nil, fmt.Errorf("normalize configuration: %w", err)
	}
	if err := cfg.Prepare(); err != nil {
		return nil, err
	}
	manager, err := core.NewManager(cfg.ArtifactDir, cfg.LeaseTTL, cfg.EventLimit)
	if err != nil {
		return nil, err
	}
	bridgeToken, err := bridgeauth.LoadOrCreateToken(cfg.BridgeTokenPath)
	if err != nil {
		manager.Close()
		return nil, err
	}
	s := &Server{
		cfg: cfg, core: manager, bridges: bridge.NewRegistry(), bridgeToken: bridgeToken,
		bridgeAuth: make(map[*rpc.Peer]struct{}), startedAt: time.Now().UTC(), closed: make(chan struct{}),
	}
	manager.SetLeaseExpiredHook(func(lease core.Lease) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.bridges.Call(ctx, lease.BrowserInstanceID, "tab.release", protocol.MarshalResult(map[string]any{"sessionId": lease.SessionID, "tabId": lease.TabID, "leaseId": lease.LeaseID, "reason": "lease expired"}))
	})
	manager.SetConfirmationResolvedHook(func(confirmation core.Confirmation) {
		_, _ = s.bridges.Notify(confirmation.BrowserInstanceID, "bridge.event", map[string]any{"type": "confirmation.resolved", "confirmation": confirmation})
	})
	return s, nil
}

func (s *Server) Listen() error {
	if s.publicListener != nil || s.bridgeListener != nil {
		return fmt.Errorf("server already listening")
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o700); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	if err := removeStaleSocket(s.cfg.SocketPath); err != nil {
		return err
	}
	if err := removeStaleSocket(s.cfg.BridgeSocketPath); err != nil {
		return err
	}
	publicListener, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.SocketPath, err)
	}
	if err := os.Chmod(s.cfg.SocketPath, 0o600); err != nil {
		_ = publicListener.Close()
		_ = os.Remove(s.cfg.SocketPath)
		return fmt.Errorf("secure daemon socket: %w", err)
	}
	bridgeListener, err := net.Listen("unix", s.cfg.BridgeSocketPath)
	if err != nil {
		_ = publicListener.Close()
		_ = os.Remove(s.cfg.SocketPath)
		return fmt.Errorf("listen on trusted bridge %s: %w", s.cfg.BridgeSocketPath, err)
	}
	if err := os.Chmod(s.cfg.BridgeSocketPath, 0o600); err != nil {
		_ = publicListener.Close()
		_ = bridgeListener.Close()
		_ = os.Remove(s.cfg.SocketPath)
		_ = os.Remove(s.cfg.BridgeSocketPath)
		return fmt.Errorf("secure bridge socket: %w", err)
	}
	s.publicListener = publicListener
	s.bridgeListener = bridgeListener
	return nil
}

func (s *Server) Serve(ctx context.Context) error {
	if s.publicListener == nil || s.bridgeListener == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.closed:
		}
	}()
	errCh := make(chan error, 2)
	go func() { errCh <- s.acceptLoop(ctx, s.publicListener, s.handlePublic, false) }()
	go func() { errCh <- s.acceptLoop(ctx, s.bridgeListener, s.handleBridge, true) }()
	select {
	case <-ctx.Done():
		_ = s.Close()
		return nil
	case <-s.closed:
		return nil
	case err := <-errCh:
		if err != nil {
			_ = s.Close()
			return err
		}
		return nil
	}
}

func (s *Server) acceptLoop(ctx context.Context, listener net.Listener, handler rpc.Handler, trustedBridge bool) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept connection: %w", err)
		}
		if trustedBridge {
			s.bridgeClients.Add(1)
		} else {
			s.clients.Add(1)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if trustedBridge {
				defer s.bridgeClients.Add(-1)
			} else {
				defer s.clients.Add(-1)
			}
			peer := rpc.NewPeer(conn, handler)
			_ = peer.Serve(ctx)
			if trustedBridge {
				s.forgetBridgeAuthentication(peer)
				disconnected := s.bridges.Disconnect(peer)
				for _, instance := range disconnected {
					s.core.EmitEvent(core.Event{Type: "browser.disconnected", Payload: protocol.MarshalResult(map[string]any{"browserInstanceId": instance.BrowserInstanceID})})
					s.core.RevokeBrowser(instance.BrowserInstanceID, "extension disconnected")
				}
				if len(disconnected) > 0 && len(s.bridges.List()) == 0 {
					s.core.StopAll("extension disconnected")
				}
			}
		}()
	}
}

func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.publicListener != nil {
			closeErr = s.publicListener.Close()
		}
		if s.bridgeListener != nil {
			if err := s.bridgeListener.Close(); closeErr == nil {
				closeErr = err
			}
		}
		s.core.StopAll("daemon shutdown")
		s.core.Close()
		_ = os.Remove(s.cfg.SocketPath)
		_ = os.Remove(s.cfg.BridgeSocketPath)
	})
	return closeErr
}

func (s *Server) Wait() { s.wg.Wait() }

func (s *Server) SocketPath() string { return s.cfg.SocketPath }

func (s *Server) handlePublic(ctx context.Context, peer *rpc.Peer, request protocol.Request) (any, *protocol.RPCError) {
	params, rpcErr := objectParams(request.Params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	switch request.Method {
	case "daemon.hello":
		return s.daemonHello(), nil
	case "daemon.status":
		return s.daemonStatus(), nil
	case "daemon.diagnostics":
		return s.daemonDiagnostics(), nil
	case "session.open":
		return s.sessionOpen(params), nil
	case "session.get":
		return s.sessionGet(params)
	case "session.close", "session.stop":
		return s.sessionClose(ctx, request.Method, params)
	case "session.requestCapabilities":
		return s.sessionCapabilities(ctx, params)
	case "browser.list":
		return s.browserList(ctx, params)
	case "browser.stop":
		return s.browserStop(ctx, params), nil
	case "tab.claim":
		return s.tabClaim(ctx, params)
	case "tab.release":
		return s.tabRelease(ctx, params)
	case "tab.lease.renew":
		return s.tabLeaseRenew(params)
	case "tab.open":
		return s.tabOpen(ctx, request.Params, params)
	case "tab.close":
		return s.tabClose(ctx, request.Params, params)
	case "artifact.get":
		return s.artifactGet(params)
	case "artifact.readChunk":
		return s.artifactRead(params)
	case "artifact.delete":
		return s.artifactDelete(params)
	case "operation.get":
		return s.operationGet(params)
	case "operation.wait":
		return s.operationWait(ctx, params)
	case "operation.cancel":
		return s.operationCancel(params)
	case "event.next":
		return s.eventNext(ctx, params)
	case "event.replay":
		return s.eventReplay(params)
	case "confirmation.get":
		return s.confirmationGet(params)
	case "confirmation.list":
		return s.confirmationList(params)
	default:
		if isForwarded(request.Method) {
			return s.forward(ctx, request.Method, request.Params, params, requiresLease(request.Method))
		}
		return nil, protocol.NewError(protocol.CodeMethodNotFound, "METHOD_NOT_FOUND", "method is not supported", false, map[string]any{"method": request.Method})
	}
}

func (s *Server) handleBridge(ctx context.Context, peer *rpc.Peer, request protocol.Request) (any, *protocol.RPCError) {
	params, rpcErr := objectParams(request.Params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if request.Method == bridgeauth.Method {
		return s.authenticateBridgeTransport(peer, request, params)
	}
	if !s.bridgeAuthenticated(peer) {
		return nil, protocol.NewError(protocol.CodeCapabilityDenied, "PERMISSION_DENIED", "bridge transport authentication is required", false, nil)
	}
	if request.Method != "bridge.hello" && s.bridges.IDForPeer(peer) == "" {
		return nil, protocol.NewError(protocol.CodeCapabilityDenied, "PERMISSION_DENIED", "trusted bridge must complete bridge.hello first", false, nil)
	}
	switch request.Method {
	case "bridge.hello":
		return s.bridgeHello(peer, params)
	case "bridge.event":
		return s.bridgeEvent(peer, params)
	case "bridge.goodbye":
		return map[string]any{"accepted": true}, nil
	case "bridge.admin.lease.revoke":
		return s.adminRevoke(params)
	case "bridge.admin.stopAll":
		return s.adminStopAll(ctx, peer, params), nil
	case "bridge.admin.confirmation.respond":
		return s.adminConfirmation(peer, params)
	default:
		return nil, protocol.NewError(protocol.CodeMethodNotFound, "METHOD_NOT_FOUND", "trusted bridge method is not supported", false, map[string]any{"method": request.Method})
	}
}

func (s *Server) authenticateBridgeTransport(peer *rpc.Peer, request protocol.Request, params map[string]any) (any, *protocol.RPCError) {
	if protocol.IsNotification(request) || !bridgeauth.Matches(s.bridgeToken, stringField(params, "token")) {
		return nil, protocol.NewError(protocol.CodeCapabilityDenied, "PERMISSION_DENIED", "bridge transport authentication failed", false, nil)
	}
	s.bridgeAuthMu.Lock()
	s.bridgeAuth[peer] = struct{}{}
	s.bridgeAuthMu.Unlock()
	return map[string]any{"accepted": true}, nil
}

func (s *Server) bridgeAuthenticated(peer *rpc.Peer) bool {
	s.bridgeAuthMu.RLock()
	_, ok := s.bridgeAuth[peer]
	s.bridgeAuthMu.RUnlock()
	return ok
}

func (s *Server) forgetBridgeAuthentication(peer *rpc.Peer) {
	s.bridgeAuthMu.Lock()
	delete(s.bridgeAuth, peer)
	s.bridgeAuthMu.Unlock()
}

func (s *Server) daemonHello() map[string]any {
	return map[string]any{
		"name": "browserd", "protocolVersion": protocol.Version, "pid": os.Getpid(),
		"startedAt": s.startedAt, "socketPath": s.cfg.SocketPath,
	}
}

func (s *Server) daemonStatus() map[string]any {
	return map[string]any{
		"ready": true, "protocolVersion": protocol.Version, "uptimeMs": time.Since(s.startedAt).Milliseconds(),
		"connectedClients": s.clients.Load(), "bridgeConnections": s.bridgeClients.Load(), "connectedBrowsers": len(s.bridges.List()), "state": s.core.Stats(),
	}
}

func (s *Server) daemonDiagnostics() map[string]any {
	mode := "missing"
	if info, err := os.Stat(s.cfg.SocketPath); err == nil {
		mode = fmt.Sprintf("%04o", info.Mode().Perm())
	}
	bridgeMode := "missing"
	if info, err := os.Stat(s.cfg.BridgeSocketPath); err == nil {
		bridgeMode = fmt.Sprintf("%04o", info.Mode().Perm())
	}
	return map[string]any{
		"ok":           true,
		"daemon":       s.daemonHello(),
		"runtime":      map[string]any{"goVersion": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH},
		"socket":       map[string]any{"path": s.cfg.SocketPath, "mode": mode},
		"bridgeSocket": map[string]any{"path": s.cfg.BridgeSocketPath, "mode": bridgeMode},
		"browsers":     s.bridges.List(),
		"state":        s.core.Stats(),
	}
}

func (s *Server) bridgeHello(peer *rpc.Peer, params map[string]any) (any, *protocol.RPCError) {
	raw, _ := json.Marshal(params)
	var hello bridge.Hello
	if err := json.Unmarshal(raw, &hello); err != nil {
		return nil, protocol.InvalidParams("invalid bridge hello", nil)
	}
	instance, rpcErr := s.bridges.Register(peer, hello)
	if rpcErr != nil {
		return nil, rpcErr
	}
	s.core.EmitEvent(core.Event{Type: "browser.connected", Payload: protocol.MarshalResult(map[string]any{"browserInstanceId": hello.BrowserInstanceID})})
	return map[string]any{
		"accepted": true, "protocolVersion": protocol.Version, "browser": instance,
		"limits": map[string]any{"nativeMessageChunkBytes": 512 * 1024, "artifactChunkBytes": 512 * 1024},
	}, nil
}

func (s *Server) bridgeEvent(peer *rpc.Peer, params map[string]any) (any, *protocol.RPCError) {
	s.bridges.Touch(peer)
	eventType := stringField(params, "type")
	if eventType == "" {
		return nil, protocol.InvalidParams("bridge event type is required", nil)
	}
	payload := protocol.MarshalResult(params["payload"])
	var confirmation *core.Confirmation
	var storedArtifact *core.Artifact
	if eventType == "confirmation.requested" {
		browserInstanceID := s.bridges.IDForPeer(peer)
		created, rpcErr := s.core.CreateActionConfirmation(stringField(params, "sessionId"), stringField(params, "tabId"), browserInstanceID, payload, durationMS(params, "timeoutMs", 2*time.Minute))
		if rpcErr != nil {
			return nil, rpcErr
		}
		confirmation = &created
		payload = protocol.MarshalResult(created)
		_, _ = s.bridges.Notify(browserInstanceID, "bridge.event", map[string]any{"type": "confirmation.requested", "confirmation": created})
	}
	if eventType == "artifact.created" {
		artifact, rpcErr := s.storeBridgeArtifact(params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		storedArtifact = &artifact
		payload = protocol.MarshalResult(artifact)
	}
	var epoch *uint64
	if value, ok := uintField(params, "documentEpoch"); ok {
		epoch = &value
	}
	event := s.core.EmitEvent(core.Event{
		Type: eventType, SessionID: stringField(params, "sessionId"), TabID: stringField(params, "tabId"), DocumentEpoch: epoch, Payload: payload,
	})
	result := map[string]any{"accepted": true, "seq": event.Seq}
	if confirmation != nil {
		result["confirmation"] = confirmation
	}
	if storedArtifact != nil {
		result["artifact"] = storedArtifact
	}
	return result, nil
}

func (s *Server) storeBridgeArtifact(params map[string]any) (core.Artifact, *protocol.RPCError) {
	payload, ok := params["payload"].(map[string]any)
	if !ok {
		return core.Artifact{}, protocol.InvalidParams("artifact.created payload must be an object", nil)
	}
	encoded := stringField(payload, "dataBase64")
	if encoded == "" {
		return core.Artifact{}, protocol.InvalidParams("artifact.created payload requires dataBase64", nil)
	}
	return s.core.StoreArtifactBase64(stringField(params, "sessionId"), stringField(params, "tabId"), stringField(payload, "kind"), stringField(payload, "mimeType"), stringField(payload, "fileName"), encoded, durationMS(payload, "ttlMs", 24*time.Hour))
}

func (s *Server) sessionOpen(params map[string]any) core.Session {
	// Protected capabilities are never granted by an untrusted public request.
	// Callers must use session.requestCapabilities and wait for trusted UI
	// approval on the separate bridge socket.
	return s.core.OpenSession(stringField(params, "name"), stringField(params, "clientId"))
}

func (s *Server) browserList(ctx context.Context, params map[string]any) (any, *protocol.RPCError) {
	instances := s.bridges.List()
	if len(instances) == 0 {
		return map[string]any{"browsers": []any{}}, nil
	}
	requested := stringField(params, "browserInstanceId")
	if requested != "" {
		result, rpcErr := s.callBridge(ctx, "browser.list", protocol.MarshalResult(params), params)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return json.RawMessage(result), nil
	}
	browsers := make([]any, 0, len(instances))
	for _, instance := range instances {
		callParams := cloneMap(params)
		callParams["browserInstanceId"] = instance.BrowserInstanceID
		result, rpcErr := s.callBridge(ctx, "browser.list", protocol.MarshalResult(callParams), callParams)
		if rpcErr != nil {
			return nil, rpcErr
		}
		var payload struct {
			Browsers []map[string]any `json:"browsers"`
		}
		if json.Unmarshal(result, &payload) != nil {
			return nil, protocol.NewError(protocol.CodeInternalError, "INTERNAL", "extension returned an invalid browser list", false, map[string]any{"browserInstanceId": instance.BrowserInstanceID})
		}
		for _, browser := range payload.Browsers {
			browser["browserInstanceId"] = instance.BrowserInstanceID
			browsers = append(browsers, browser)
		}
	}
	return map[string]any{"browsers": browsers}, nil
}

func (s *Server) sessionGet(params map[string]any) (any, *protocol.RPCError) {
	id := stringField(params, "sessionId")
	if id == "" {
		return nil, protocol.InvalidParams("sessionId is required", nil)
	}
	return s.core.GetSession(id)
}

func (s *Server) sessionCapabilities(ctx context.Context, params map[string]any) (any, *protocol.RPCError) {
	_ = ctx
	browserInstanceID := stringField(params, "browserInstanceId")
	if browserInstanceID == "" {
		browserInstanceID = s.bridges.DefaultID()
	}
	if browserInstanceID == "" {
		return nil, protocol.NewError(protocol.CodeBridgeUnavailable, "EXTENSION_DISCONNECTED", "trusted Chrome UI is not connected", true, nil)
	}
	confirmation, rpcErr := s.core.CreateCapabilityConfirmation(stringField(params, "sessionId"), browserInstanceID, stringSlice(params["capabilities"]), durationMS(params, "timeoutMs", 2*time.Minute))
	if rpcErr != nil {
		return nil, rpcErr
	}
	if _, err := s.bridges.Notify(browserInstanceID, "bridge.event", map[string]any{"type": "confirmation.requested", "confirmation": confirmation}); err != nil {
		return nil, protocol.NewError(protocol.CodeBridgeUnavailable, "EXTENSION_DISCONNECTED", "trusted Chrome UI is not connected", true, map[string]any{"browserInstanceId": browserInstanceID})
	}
	return confirmation, nil
}

func (s *Server) sessionClose(ctx context.Context, method string, params map[string]any) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return nil, protocol.InvalidParams("sessionId is required", nil)
	}
	reason := stringField(params, "reason")
	if reason == "" {
		reason = method
	}
	released, rpcErr := s.core.CloseSession(sessionID, reason)
	if rpcErr != nil {
		return nil, rpcErr
	}
	s.releaseLeasesOnExtension(ctx, stringField(params, "browserInstanceId"), released, reason)
	return map[string]any{"sessionId": sessionID, "status": "closed", "releasedLeases": released}, nil
}

func (s *Server) tabClaim(ctx context.Context, params map[string]any) (any, *protocol.RPCError) {
	sessionID, tabID := stringField(params, "sessionId"), stringField(params, "tabId")
	if sessionID == "" || tabID == "" {
		return nil, protocol.InvalidParams("sessionId and tabId are required", nil)
	}
	browserInstanceID := stringField(params, "browserInstanceId")
	if browserInstanceID == "" {
		browserInstanceID = s.bridges.DefaultID()
	}
	if browserInstanceID == "" {
		return nil, protocol.NewError(protocol.CodeBridgeUnavailable, "EXTENSION_DISCONNECTED", "no compatible Chrome extension is connected", true, nil)
	}
	lease, rpcErr := s.core.ClaimTab(sessionID, tabID, durationMS(params, "leaseTtlMs", 0))
	if rpcErr != nil {
		return nil, rpcErr
	}
	claimedLease := lease
	lease, rpcErr = s.core.BindLeaseBrowser(sessionID, tabID, claimedLease.LeaseID, browserInstanceID)
	if rpcErr != nil {
		if claimedLease.BrowserInstanceID == "" {
			_, _ = s.core.ReleaseTab(sessionID, tabID, claimedLease.LeaseID, "browser binding rejected")
		}
		return nil, rpcErr
	}
	forwardParams := cloneMap(params)
	forwardParams["leaseId"] = lease.LeaseID
	forwardParams["browserInstanceId"] = browserInstanceID
	if session, sessionErr := s.core.GetSession(sessionID); sessionErr == nil {
		forwardParams["session"] = map[string]any{"sessionId": session.SessionID, "name": session.Name, "clientId": session.ClientID}
	}
	result, bridgeErr := s.callBridge(ctx, "tab.claim", protocol.MarshalResult(forwardParams), forwardParams)
	if bridgeErr != nil {
		_, _ = s.core.ReleaseTab(sessionID, tabID, lease.LeaseID, "claim rejected by extension")
		return nil, bridgeErr
	}
	return mergeResult(result, map[string]any{"leaseId": lease.LeaseID, "expiresAt": lease.ExpiresAt}), nil
}

func (s *Server) tabRelease(ctx context.Context, params map[string]any) (any, *protocol.RPCError) {
	sessionID, tabID, leaseID := leaseFields(params)
	current, rpcErr := s.core.RenewLease(sessionID, tabID, leaseID, 0)
	if rpcErr != nil {
		return nil, rpcErr
	}
	requestedBrowserID := stringField(params, "browserInstanceId")
	if requestedBrowserID != "" && requestedBrowserID != current.BrowserInstanceID {
		return nil, protocol.NewError(protocol.CodeLeaseConflict, "LEASE_CONFLICT", "lease belongs to a different Chrome instance", false, map[string]any{"tabId": tabID})
	}
	bridgeParams := cloneMap(params)
	bridgeParams["browserInstanceId"] = current.BrowserInstanceID
	result, bridgeErr := s.callBridge(ctx, "tab.release", protocol.MarshalResult(bridgeParams), bridgeParams)
	if bridgeErr != nil {
		return nil, bridgeErr
	}
	released, rpcErr := s.core.ReleaseTab(sessionID, tabID, leaseID, stringField(params, "reason"))
	if rpcErr != nil {
		return nil, rpcErr
	}
	return mergeResult(result, map[string]any{"released": true, "lease": released}), nil
}

func (s *Server) tabLeaseRenew(params map[string]any) (any, *protocol.RPCError) {
	sessionID, tabID, leaseID := leaseFields(params)
	return s.core.RenewLease(sessionID, tabID, leaseID, durationMS(params, "leaseTtlMs", 0))
}

func (s *Server) tabOpen(ctx context.Context, raw json.RawMessage, params map[string]any) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if _, rpcErr := s.core.GetSession(sessionID); rpcErr != nil {
		return nil, rpcErr
	}
	result, rpcErr := s.forward(ctx, "tab.open", raw, params, false)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if tabID := tabIDFromResult(result); tabID != "" {
		if err := s.core.MarkOwnedTab(sessionID, tabID); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Server) tabClose(ctx context.Context, raw json.RawMessage, params map[string]any) (any, *protocol.RPCError) {
	sessionID, tabID, leaseID := leaseFields(params)
	if rpcErr := s.core.ValidateLease(sessionID, tabID, leaseID); rpcErr != nil {
		return nil, rpcErr
	}
	if !s.core.CanCloseTab(sessionID, tabID) {
		return nil, protocol.NewError(protocol.CodeCapabilityDenied, "PERMISSION_DENIED", "only tabs created by this session may be closed", false, map[string]any{"sessionId": sessionID, "tabId": tabID})
	}
	result, rpcErr := s.forward(ctx, "tab.close", raw, params, true)
	if rpcErr == nil {
		_, _ = s.core.ReleaseTab(sessionID, tabID, leaseID, "tab closed")
	}
	return result, rpcErr
}

func (s *Server) forward(ctx context.Context, method string, raw json.RawMessage, params map[string]any, leaseRequired bool) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if sessionID != "" {
		if _, rpcErr := s.core.GetSession(sessionID); rpcErr != nil {
			return nil, rpcErr
		}
	}
	if capability := capabilityForRequest(method, params); capability != "" {
		if sessionID == "" {
			return nil, protocol.InvalidParams("sessionId is required for capability-protected methods", map[string]any{"method": method})
		}
		if rpcErr := s.core.RequireCapability(sessionID, capability); rpcErr != nil {
			return nil, rpcErr
		}
	}
	if leaseRequired {
		sessionID, tabID, leaseID := leaseFields(params)
		if rpcErr := s.core.ValidateLease(sessionID, tabID, leaseID); rpcErr != nil {
			return nil, rpcErr
		}
		renewed, renewErr := s.core.RenewLease(sessionID, tabID, leaseID, 0)
		if renewErr != nil {
			return nil, renewErr
		}
		requestedBrowserID := stringField(params, "browserInstanceId")
		if requestedBrowserID != "" && requestedBrowserID != renewed.BrowserInstanceID {
			return nil, protocol.NewError(protocol.CodeLeaseConflict, "LEASE_CONFLICT", "lease belongs to a different Chrome instance", false, map[string]any{"tabId": tabID})
		}
		if renewed.BrowserInstanceID != "" {
			params = cloneMap(params)
			params["browserInstanceId"] = renewed.BrowserInstanceID
			raw = protocol.MarshalResult(params)
		}
	}
	if method == "fileChooser.setFiles" {
		files, rpcErr := validateUploadFiles(params, s.cfg.UploadRoots)
		if rpcErr != nil {
			return nil, rpcErr
		}
		// Forward canonical paths only. This prevents the extension from later
		// resolving a user-supplied symlink to a different target than the one
		// checked at the daemon policy boundary.
		params = cloneMap(params)
		params["files"] = files
		delete(params, "paths")
		raw = protocol.MarshalResult(params)
	}
	operationID := stringField(params, "operationId")
	if mutatingMethod(method) && (operationID == "" || !strings.HasPrefix(operationID, "op_") || len(operationID) > 160) {
		return nil, protocol.InvalidParams(method+" requires a caller-generated operationId beginning with op_", nil)
	}
	if requiresDocumentEpoch(method) {
		if _, ok := uintField(params, "expectedDocumentEpoch"); !ok {
			return nil, protocol.InvalidParams(method+" requires expectedDocumentEpoch", nil)
		}
	}
	tabID := stringField(params, "tabId")
	call := func(callCtx context.Context) (json.RawMessage, *protocol.RPCError) {
		result, rpcErr := s.core.RunTab(callCtx, tabID, func(tabCtx context.Context) (json.RawMessage, *protocol.RPCError) {
			callRaw, callParams := raw, params
			if method == "action.perform" {
				var gateErr *protocol.RPCError
				callRaw, callParams, gateErr = s.authorizeAction(tabCtx, callRaw, callParams)
				if gateErr != nil {
					return nil, gateErr
				}
			}
			return s.callBridge(tabCtx, method, callRaw, callParams)
		})
		if rpcErr != nil && operationID != "" && mutatingMethod(method) {
			switch protocol.ErrorKind(rpcErr) {
			case "TIMEOUT", "CANCELLED", "EXTENSION_DISCONNECTED", "CHROME_DISCONNECTED":
				rpcErr = protocol.WithEffect(rpcErr, "possible", map[string]any{"operationId": operationID, "sessionId": sessionID, "tabId": tabID})
			}
		}
		return result, rpcErr
	}
	if operationID != "" {
		fingerprintParams := cloneMap(params)
		delete(fingerprintParams, "confirmationId")
		delete(fingerprintParams, "__approvedRequestHash")
		result, rpcErr, replayed := s.core.ExecuteOperation(ctx, sessionID, tabID, operationID, fingerprintParams, call)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return map[string]any{"operationId": operationID, "replayed": replayed, "result": result}, nil
	}
	result, rpcErr := call(ctx)
	return result, rpcErr
}

func (s *Server) authorizeAction(ctx context.Context, raw json.RawMessage, params map[string]any) (json.RawMessage, map[string]any, *protocol.RPCError) {
	browserInstanceID := stringField(params, "browserInstanceId")
	if browserInstanceID == "" {
		browserInstanceID = s.bridges.DefaultID()
	}
	if browserInstanceID == "" {
		return nil, nil, protocol.NewError(protocol.CodeBridgeUnavailable, "EXTENSION_DISCONNECTED", "no compatible Chrome extension is connected", true, nil)
	}
	boundParams := cloneMap(params)
	boundParams["browserInstanceId"] = browserInstanceID
	boundRaw := protocol.MarshalResult(boundParams)
	preflightRaw, rpcErr := s.callBridge(ctx, "action.preflight", boundRaw, boundParams)
	if rpcErr != nil {
		return nil, nil, rpcErr
	}
	preflight, parseErr := objectParams(preflightRaw)
	if parseErr != nil {
		return nil, nil, protocol.NewError(protocol.CodeInternalError, "INTERNAL", "extension returned an invalid action preflight", false, nil)
	}
	required, _ := preflight["confirmationRequired"].(bool)
	if !required {
		return boundRaw, boundParams, nil
	}
	requestHash := stringField(preflight, "requestHash")
	if requestHash == "" {
		return nil, nil, protocol.NewError(protocol.CodeInternalError, "INTERNAL", "extension omitted the action request hash", false, nil)
	}
	sessionID, tabID := stringField(boundParams, "sessionId"), stringField(boundParams, "tabId")
	confirmationID := stringField(boundParams, "confirmationId")
	if confirmationID == "" {
		metadata := map[string]any{
			"requestHash": requestHash,
			"operationId": stringField(boundParams, "operationId"),
			"title":       stringField(preflight, "title"),
			"summary":     stringField(preflight, "summary"),
			"origin":      stringField(preflight, "origin"),
		}
		confirmation, createErr := s.core.CreateActionConfirmation(
			sessionID,
			tabID,
			browserInstanceID,
			protocol.MarshalResult(metadata),
			durationMS(boundParams, "confirmationTimeoutMs", 2*time.Minute),
		)
		if createErr != nil {
			return nil, nil, createErr
		}
		if _, err := s.bridges.Notify(browserInstanceID, "bridge.event", map[string]any{"type": "confirmation.requested", "confirmation": confirmation}); err != nil {
			return nil, nil, protocol.NewError(protocol.CodeBridgeUnavailable, "EXTENSION_DISCONNECTED", "trusted confirmation UI is unavailable", true, map[string]any{"browserInstanceId": browserInstanceID})
		}
		return nil, nil, protocol.NewError(protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED", "approve or deny this exact action in the trusted extension UI", true, map[string]any{
			"confirmationId": confirmation.ConfirmationID,
			"expiresAt":      confirmation.ExpiresAt,
			"operationId":    stringField(boundParams, "operationId"),
			"sessionId":      sessionID,
			"tabId":          tabID,
		})
	}
	if _, authErr := s.core.AuthorizeActionConfirmation(confirmationID, sessionID, tabID, browserInstanceID, requestHash); authErr != nil {
		return nil, nil, authErr
	}
	if consumeErr := s.core.ConsumeActionConfirmation(confirmationID); consumeErr != nil {
		return nil, nil, consumeErr
	}
	boundParams["__approvedRequestHash"] = requestHash
	return protocol.MarshalResult(boundParams), boundParams, nil
}

func mutatingMethod(method string) bool {
	switch method {
	case "tab.open", "tab.activate", "tab.close", "tab.navigate", "tab.back", "tab.forward", "tab.reload",
		"action.perform", "dialog.respond", "clipboard.write", "fileChooser.setFiles", "content.export", "pageAssets.export",
		"secureInput.request", "unsafe.evaluate", "unsafe.cdp.send":
		return true
	default:
		return false
	}
}

func requiresDocumentEpoch(method string) bool {
	switch method {
	case "tab.navigate", "tab.back", "tab.forward", "tab.reload", "action.perform", "dialog.respond",
		"fileChooser.setFiles", "secureInput.request", "unsafe.evaluate", "unsafe.cdp.send":
		return true
	default:
		return false
	}
}

func (s *Server) callBridge(ctx context.Context, method string, raw json.RawMessage, params map[string]any) (json.RawMessage, *protocol.RPCError) {
	timeout := durationMS(params, "timeoutMs", 30*time.Second)
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.bridges.Call(callCtx, stringField(params, "browserInstanceId"), method, raw)
}

func (s *Server) browserStop(ctx context.Context, params map[string]any) map[string]any {
	reason := stringField(params, "reason")
	if reason == "" {
		reason = "browser.stop"
	}
	var released []core.Lease
	if sessionID := stringField(params, "sessionId"); sessionID != "" {
		released, _ = s.core.CloseSession(sessionID, reason)
		s.releaseLeasesOnExtension(ctx, stringField(params, "browserInstanceId"), released, reason)
		return map[string]any{"stopped": true, "releasedLeases": released}
	}
	browserInstanceID := stringField(params, "browserInstanceId")
	instanceIDs := make([]string, 0)
	if browserInstanceID != "" {
		released = s.core.RevokeBrowser(browserInstanceID, reason)
		instanceIDs = append(instanceIDs, browserInstanceID)
	} else {
		released = s.core.StopAll(reason)
		for _, instance := range s.bridges.List() {
			instanceIDs = append(instanceIDs, instance.BrowserInstanceID)
		}
	}
	result := map[string]any{"stopped": true, "releasedLeases": released}
	warnings := make([]*protocol.RPCError, 0)
	for _, instanceID := range instanceIDs {
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, bridgeErr := s.bridges.Call(callCtx, instanceID, "browser.stop", protocol.MarshalResult(map[string]any{"browserInstanceId": instanceID, "reason": reason}))
		cancel()
		if bridgeErr != nil {
			warnings = append(warnings, bridgeErr)
		}
	}
	if len(warnings) > 0 {
		result["bridgeWarnings"] = warnings
	}
	return result
}

func (s *Server) releaseLeasesOnExtension(ctx context.Context, browserInstanceID string, leases []core.Lease, reason string) {
	for _, lease := range leases {
		leaseBrowserID := lease.BrowserInstanceID
		if leaseBrowserID == "" {
			leaseBrowserID = browserInstanceID
		}
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, _ = s.bridges.Call(callCtx, leaseBrowserID, "tab.release", protocol.MarshalResult(map[string]any{
			"sessionId": lease.SessionID, "tabId": lease.TabID, "leaseId": lease.LeaseID, "reason": reason,
		}))
		cancel()
	}
}

func (s *Server) stopAll(params map[string]any) map[string]any {
	reason := stringField(params, "reason")
	if reason == "" {
		reason = "admin stop"
	}
	return map[string]any{"stopped": true, "releasedLeases": s.core.StopAll(reason)}
}

func (s *Server) adminStopAll(ctx context.Context, source *rpc.Peer, params map[string]any) map[string]any {
	result := s.stopAll(params)
	reason := stringField(params, "reason")
	if reason == "" {
		reason = "admin stop"
	}
	for _, instance := range s.bridges.List() {
		if s.bridges.PeerOwns(source, instance.BrowserInstanceID) {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, _ = s.bridges.Call(callCtx, instance.BrowserInstanceID, "browser.stop", protocol.MarshalResult(map[string]any{"reason": reason}))
		cancel()
	}
	return result
}

func (s *Server) adminRevoke(params map[string]any) (any, *protocol.RPCError) {
	return s.core.RevokeTab(stringField(params, "tabId"), stringField(params, "reason"))
}

func (s *Server) adminConfirmation(peer *rpc.Peer, params map[string]any) (any, *protocol.RPCError) {
	confirmation, rpcErr := s.core.GetConfirmation(stringField(params, "confirmationId"))
	if rpcErr != nil {
		return nil, rpcErr
	}
	if confirmation.BrowserInstanceID == "" || !s.bridges.PeerOwns(peer, confirmation.BrowserInstanceID) {
		return nil, protocol.NewError(protocol.CodeCapabilityDenied, "PERMISSION_DENIED", "confirmation belongs to a different trusted Chrome instance", false, nil)
	}
	resolved, rpcErr := s.core.ResolveConfirmation(confirmation.ConfirmationID, stringField(params, "decision"))
	if rpcErr != nil {
		return nil, rpcErr
	}
	_, _ = s.bridges.Notify(resolved.BrowserInstanceID, "bridge.event", map[string]any{"type": "confirmation.resolved", "confirmation": resolved})
	return resolved, nil
}

func (s *Server) confirmationGet(params map[string]any) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return nil, protocol.InvalidParams("sessionId is required", nil)
	}
	confirmation, rpcErr := s.core.GetConfirmation(stringField(params, "confirmationId"))
	if rpcErr != nil {
		return nil, rpcErr
	}
	if confirmation.SessionID != sessionID {
		return nil, protocol.NewError(protocol.CodeCapabilityDenied, "PERMISSION_DENIED", "confirmation belongs to a different session", false, nil)
	}
	return confirmation, nil
}

func (s *Server) confirmationList(params map[string]any) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return nil, protocol.InvalidParams("sessionId is required", nil)
	}
	if _, rpcErr := s.core.GetSession(sessionID); rpcErr != nil {
		return nil, rpcErr
	}
	return map[string]any{"confirmations": s.core.ListConfirmations(sessionID, stringField(params, "status"))}, nil
}

func (s *Server) artifactGet(params map[string]any) (any, *protocol.RPCError) {
	a, rpcErr := s.core.GetArtifact(stringField(params, "artifactId"))
	if rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := s.authorizeArtifact(params, a); rpcErr != nil {
		return nil, rpcErr
	}
	result := protocolResultMap(a)
	if params["includeLocalPath"] == true {
		if rpcErr := s.core.RequireCapability(stringField(params, "sessionId"), "artifact.localPath"); rpcErr != nil {
			return nil, rpcErr
		}
		path, rpcErr := s.core.ArtifactLocalPath(a.ArtifactID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		result["localPath"] = path
	}
	return result, nil
}

func (s *Server) artifactRead(params map[string]any) (any, *protocol.RPCError) {
	metadata, rpcErr := s.core.GetArtifact(stringField(params, "artifactId"))
	if rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := s.authorizeArtifact(params, metadata); rpcErr != nil {
		return nil, rpcErr
	}
	offset, _ := int64Field(params, "offset")
	length, _ := int64Field(params, "length")
	data, artifact, rpcErr := s.core.ReadArtifactChunk(metadata.ArtifactID, offset, length)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return map[string]any{
		"artifactId": artifact.ArtifactID, "offset": offset, "length": len(data),
		"eof": offset+int64(len(data)) >= artifact.Size, "dataBase64": base64.StdEncoding.EncodeToString(data),
	}, nil
}

func (s *Server) artifactDelete(params map[string]any) (any, *protocol.RPCError) {
	id := stringField(params, "artifactId")
	artifact, rpcErr := s.core.GetArtifact(id)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := s.authorizeArtifact(params, artifact); rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := s.core.DeleteArtifact(id); rpcErr != nil {
		return nil, rpcErr
	}
	return map[string]any{"artifactId": id, "deleted": true}, nil
}

func (s *Server) authorizeArtifact(params map[string]any, artifact core.Artifact) *protocol.RPCError {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return protocol.InvalidParams("sessionId is required for artifact access", nil)
	}
	if artifact.SessionID != sessionID {
		return protocol.NewError(protocol.CodeCapabilityDenied, "PERMISSION_DENIED", "artifact belongs to a different session", false, map[string]any{"artifactId": artifact.ArtifactID, "sessionId": sessionID})
	}
	if _, rpcErr := s.core.GetSession(sessionID); rpcErr != nil {
		return rpcErr
	}
	return nil
}

func protocolResultMap(value any) map[string]any {
	raw := protocol.MarshalResult(value)
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	return result
}

func (s *Server) operationGet(params map[string]any) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return nil, protocol.InvalidParams("sessionId is required", nil)
	}
	return s.core.GetOperation(sessionID, stringField(params, "operationId"))
}

func (s *Server) operationWait(ctx context.Context, params map[string]any) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return nil, protocol.InvalidParams("sessionId is required", nil)
	}
	waitCtx, cancel := context.WithTimeout(ctx, durationMS(params, "timeoutMs", 30*time.Second))
	defer cancel()
	return s.core.WaitOperation(waitCtx, sessionID, stringField(params, "operationId"))
}

func (s *Server) operationCancel(params map[string]any) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return nil, protocol.InvalidParams("sessionId is required", nil)
	}
	return s.core.CancelOperation(sessionID, stringField(params, "operationId"))
}

func (s *Server) eventNext(ctx context.Context, params map[string]any) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return nil, protocol.InvalidParams("sessionId is required", nil)
	}
	if _, rpcErr := s.core.GetSession(sessionID); rpcErr != nil {
		return nil, rpcErr
	}
	waitCtx, cancel := context.WithTimeout(ctx, durationMS(params, "timeoutMs", 30*time.Second))
	defer cancel()
	return s.core.NextEvent(waitCtx, eventFilter(params))
}

func (s *Server) eventReplay(params map[string]any) (any, *protocol.RPCError) {
	sessionID := stringField(params, "sessionId")
	if sessionID == "" {
		return nil, protocol.InvalidParams("sessionId is required", nil)
	}
	if _, rpcErr := s.core.GetSession(sessionID); rpcErr != nil {
		return nil, rpcErr
	}
	limit := int(numberField(params, "limit", 100))
	return map[string]any{"events": s.core.ReplayEvents(eventFilter(params), limit)}, nil
}

func eventFilter(params map[string]any) core.EventFilter {
	after, _ := uintField(params, "afterSeq")
	return core.EventFilter{AfterSeq: after, Types: core.DecodeStringSet(stringSlice(params["types"])), SessionID: stringField(params, "sessionId"), TabID: stringField(params, "tabId")}
}

func objectParams(raw json.RawMessage) (map[string]any, *protocol.RPCError) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	var params map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&params); err != nil || params == nil {
		return nil, protocol.InvalidParams("params must be a JSON object", nil)
	}
	return params, nil
}

func isForwarded(method string) bool {
	for _, prefix := range []string{"browser.", "tab.", "observation.", "locator.", "action.", "condition.", "dialog.", "clipboard.", "fileChooser.", "download.", "content.", "pageAssets.", "secureInput.", "unsafe."} {
		if strings.HasPrefix(method, prefix) {
			return true
		}
	}
	return false
}

func requiresLease(method string) bool {
	switch method {
	case "browser.history.query", "tab.list", "tab.get", "tab.open", "tab.claim", "browser.stop":
		return false
	default:
		return !strings.HasPrefix(method, "browser.")
	}
}

func capabilityFor(method string) string {
	switch {
	case method == "browser.history.query":
		return "history.read"
	case method == "clipboard.read":
		return "clipboard.read"
	case method == "clipboard.write":
		return "clipboard.write"
	case method == "fileChooser.setFiles":
		return "files.upload"
	case strings.HasPrefix(method, "download."):
		return "files.download"
	case strings.HasPrefix(method, "secureInput."):
		return "secureInput"
	case method == "unsafe.evaluate":
		return "unsafe.evaluate"
	case method == "unsafe.cdp.send":
		return "unsafe.cdp"
	default:
		return ""
	}
}

func capabilityForRequest(method string, params map[string]any) string {
	if method == "action.perform" {
		if action, ok := params["action"].(map[string]any); ok && stringField(action, "type") == "downloadMedia" {
			return "files.download"
		}
		if expectations, ok := params["expect"].([]any); ok {
			for _, expectation := range expectations {
				if item, ok := expectation.(map[string]any); ok && stringField(item, "type") == "download" {
					return "files.download"
				}
			}
		}
	}
	return capabilityFor(method)
}

func leaseFields(params map[string]any) (string, string, string) {
	return stringField(params, "sessionId"), stringField(params, "tabId"), stringField(params, "leaseId")
}

func stringField(params map[string]any, key string) string {
	if value, ok := params[key].(string); ok {
		return value
	}
	return ""
}

func stringSlice(value any) []string {
	switch values := value.(type) {
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	case []string:
		return values
	default:
		return nil
	}
}

func numberField(params map[string]any, key string, fallback float64) float64 {
	switch value := params[key].(type) {
	case json.Number:
		parsed, err := value.Float64()
		if err == nil {
			return parsed
		}
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	}
	return fallback
}

func durationMS(params map[string]any, key string, fallback time.Duration) time.Duration {
	value := numberField(params, key, -1)
	if value < 0 {
		return fallback
	}
	return time.Duration(value * float64(time.Millisecond))
}

func uintField(params map[string]any, key string) (uint64, bool) {
	value := numberField(params, key, -1)
	if value < 0 {
		return 0, false
	}
	return uint64(value), true
}

func int64Field(params map[string]any, key string) (int64, bool) {
	value := numberField(params, key, -1)
	if value < 0 {
		return 0, false
	}
	return int64(value), true
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func mergeResult(raw json.RawMessage, extra map[string]any) map[string]any {
	result := make(map[string]any)
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &result)
		if len(result) == 0 {
			result["extensionResult"] = raw
		}
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}

func tabIDFromResult(value any) string {
	raw := protocol.MarshalResult(value)
	var direct map[string]any
	if json.Unmarshal(raw, &direct) != nil {
		return ""
	}
	if tabID, ok := direct["tabId"].(string); ok {
		return tabID
	}
	if tab, ok := direct["tab"].(map[string]any); ok {
		if tabID, ok := tab["tabId"].(string); ok {
			return tabID
		}
	}
	if result, ok := direct["result"].(map[string]any); ok {
		if tabID, ok := result["tabId"].(string); ok {
			return tabID
		}
	}
	return ""
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket path %s", path)
	}
	conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("browserd is already listening at %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}

func SortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for key, enabled := range values {
		if enabled {
			result = append(result, key)
		}
	}
	sort.Strings(result)
	return result
}
