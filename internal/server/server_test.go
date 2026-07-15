package server

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cc-hchi/browser-control/internal/bridgeauth"
	"github.com/cc-hchi/browser-control/internal/config"
	"github.com/cc-hchi/browser-control/internal/core"
	"github.com/cc-hchi/browser-control/internal/protocol"
	"github.com/cc-hchi/browser-control/internal/rpc"
)

func TestPublicAndTrustedBridgeRPCSurfacesAreIsolated(t *testing.T) {
	server, public, trusted, _ := startTestServer(t)
	_ = server
	registerBridge(t, trusted, "browser-surface-test")

	if result, rpcErr := call(t, public, "daemon.hello", map[string]any{}); rpcErr != nil {
		t.Fatalf("public daemon.hello error = %v", rpcErr)
	} else {
		var hello map[string]any
		if err := json.Unmarshal(result, &hello); err != nil || hello["name"] != "browserd" {
			t.Fatalf("public daemon.hello result = %s, error = %v", result, err)
		}
	}

	for _, method := range []string{bridgeauth.Method, "bridge.hello", "bridge.event", "bridge.admin.stopAll", "bridge.admin.lease.revoke", "bridge.admin.confirmation.respond"} {
		_, rpcErr := call(t, public, method, map[string]any{})
		assertServerRPCError(t, rpcErr, protocol.CodeMethodNotFound, "METHOD_NOT_FOUND")
	}

	for _, method := range []string{"daemon.hello", "session.open", "browser.list", "tab.claim", "confirmation.get"} {
		_, rpcErr := call(t, trusted, method, map[string]any{})
		assertServerRPCError(t, rpcErr, protocol.CodeMethodNotFound, "METHOD_NOT_FOUND")
	}
}

func TestBridgeTransportRequiresCorrectTokenBeforeHello(t *testing.T) {
	server, public, _, cfg := startTestServer(t)
	_ = public
	unauthenticated, err := rpc.Dial(context.Background(), "unix", cfg.BridgeSocketPath, nil)
	if err != nil {
		t.Fatalf("dial unauthenticated bridge: %v", err)
	}
	t.Cleanup(func() { _ = unauthenticated.Close() })

	for method, params := range map[string]any{
		"bridge.hello":                      testBridgeHello("browser-unauthenticated"),
		"bridge.event":                      map[string]any{"type": "tab.updated"},
		"bridge.admin.stopAll":              map[string]any{},
		"bridge.admin.confirmation.respond": map[string]any{"confirmationId": "none", "decision": "approve"},
	} {
		_, rpcErr := call(t, unauthenticated, method, params)
		assertServerRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")
	}
	_, rpcErr := call(t, unauthenticated, bridgeauth.Method, map[string]any{"token": "incorrect"})
	assertServerRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")
	_, rpcErr = call(t, unauthenticated, "bridge.hello", testBridgeHello("browser-still-unauthenticated"))
	assertServerRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")

	authenticateBridge(t, unauthenticated, cfg)
	if _, rpcErr := call(t, unauthenticated, "bridge.hello", testBridgeHello("browser-authenticated")); rpcErr != nil {
		t.Fatalf("bridge.hello after authentication: %v", rpcErr)
	}

	token, err := bridgeauth.LoadToken(cfg.BridgeTokenPath)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := json.Marshal(server.daemonDiagnostics())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(diagnostics), bridgeauth.EncodedToken(token)) {
		t.Fatal("daemon diagnostics exposed the bridge token")
	}
}

func TestNativeHostAuthenticationHandshakeUnlocksBridgeRPC(t *testing.T) {
	_, _, _, cfg := startTestServer(t)
	conn, err := net.Dial("unix", cfg.BridgeSocketPath)
	if err != nil {
		t.Fatalf("dial bridge socket: %v", err)
	}
	token, err := bridgeauth.LoadToken(cfg.BridgeTokenPath)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("load bridge token: %v", err)
	}
	if err := bridgeauth.Authenticate(conn, token, time.Second); err != nil {
		_ = conn.Close()
		t.Fatalf("native host authentication handshake: %v", err)
	}
	peer := rpc.NewPeer(conn, nil)
	go func() { _ = peer.Serve(context.Background()) }()
	t.Cleanup(func() { _ = peer.Close() })
	if _, rpcErr := call(t, peer, "bridge.hello", testBridgeHello("browser-native-handshake")); rpcErr != nil {
		t.Fatalf("bridge.hello after native handshake: %v", rpcErr)
	}
}

func TestOnlyOwningTrustedBridgeCanApproveCapabilityConfirmation(t *testing.T) {
	_, public, ownerBridge, cfg := startTestServer(t)
	otherBridge, err := rpc.Dial(context.Background(), "unix", cfg.BridgeSocketPath, nil)
	if err != nil {
		t.Fatalf("dial second bridge socket: %v", err)
	}
	t.Cleanup(func() { _ = otherBridge.Close() })
	authenticateBridge(t, otherBridge, cfg)

	registerBridge(t, ownerBridge, "browser-owner")
	registerBridge(t, otherBridge, "browser-other")

	rawSession, rpcErr := call(t, public, "session.open", map[string]any{
		"name":         "permission-test",
		"clientId":     "test",
		"capabilities": []string{"unsafe.cdp", "clipboard.read"},
	})
	if rpcErr != nil {
		t.Fatalf("session.open error = %v", rpcErr)
	}
	var session core.Session
	if err := json.Unmarshal(rawSession, &session); err != nil {
		t.Fatalf("decode session.open: %v", err)
	}
	if len(session.Capabilities) != 0 {
		t.Fatalf("public session.open granted requested capabilities: %v", session.Capabilities)
	}

	rawConfirmation, rpcErr := call(t, public, "session.requestCapabilities", map[string]any{
		"sessionId":         session.SessionID,
		"browserInstanceId": "browser-owner",
		"capabilities":      []string{"unsafe.cdp"},
	})
	if rpcErr != nil {
		t.Fatalf("session.requestCapabilities error = %v", rpcErr)
	}
	var confirmation core.Confirmation
	if err := json.Unmarshal(rawConfirmation, &confirmation); err != nil {
		t.Fatalf("decode confirmation: %v", err)
	}
	if confirmation.Status != "pending" || confirmation.BrowserInstanceID != "browser-owner" {
		t.Fatalf("created confirmation = %+v", confirmation)
	}

	_, rpcErr = call(t, public, "bridge.admin.confirmation.respond", map[string]any{
		"confirmationId": confirmation.ConfirmationID,
		"decision":       "approve",
	})
	assertServerRPCError(t, rpcErr, protocol.CodeMethodNotFound, "METHOD_NOT_FOUND")

	_, rpcErr = call(t, otherBridge, "bridge.admin.confirmation.respond", map[string]any{
		"confirmationId": confirmation.ConfirmationID,
		"decision":       "approve",
	})
	assertServerRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")

	rawPending, rpcErr := call(t, public, "confirmation.get", map[string]any{
		"sessionId": session.SessionID, "confirmationId": confirmation.ConfirmationID,
	})
	if rpcErr != nil {
		t.Fatalf("confirmation.get error = %v", rpcErr)
	}
	var pending core.Confirmation
	if err := json.Unmarshal(rawPending, &pending); err != nil || pending.Status != "pending" {
		t.Fatalf("confirmation after rejected approvals = (%+v, %v)", pending, err)
	}

	rawApproved, rpcErr := call(t, ownerBridge, "bridge.admin.confirmation.respond", map[string]any{
		"confirmationId": confirmation.ConfirmationID,
		"decision":       "approve",
	})
	if rpcErr != nil {
		t.Fatalf("owner confirmation approval error = %v", rpcErr)
	}
	var approved core.Confirmation
	if err := json.Unmarshal(rawApproved, &approved); err != nil || approved.Status != "approved" {
		t.Fatalf("approved confirmation = (%+v, %v)", approved, err)
	}

	rawUpdated, rpcErr := call(t, public, "session.get", map[string]any{"sessionId": session.SessionID})
	if rpcErr != nil {
		t.Fatalf("session.get error = %v", rpcErr)
	}
	var updated core.Session
	if err := json.Unmarshal(rawUpdated, &updated); err != nil {
		t.Fatalf("decode session.get: %v", err)
	}
	if !updated.Capabilities["unsafe.cdp"] {
		t.Fatalf("trusted approval did not grant capability: %v", updated.Capabilities)
	}
}

func TestConsequentialActionRequiresTrustedApprovalAndReplaysOnce(t *testing.T) {
	server, public, _, cfg := startTestServer(t)
	_ = server
	var preflights atomic.Int32
	var performed atomic.Int32
	var forwardedPreflight atomic.Bool
	bridgeHandler := func(_ context.Context, _ *rpc.Peer, request protocol.Request) (any, *protocol.RPCError) {
		switch request.Method {
		case "tab.claim":
			return map[string]any{"tabId": "tab-action", "documentEpoch": 1, "claimed": true}, nil
		case "action.preflight":
			preflights.Add(1)
			return map[string]any{
				"confirmationRequired": true,
				"requestHash":          "12" + strings.Repeat("0", 62),
				"title":                "Confirm browser action",
				"summary":              "click on Save",
				"origin":               "https://example.test",
			}, nil
		case "action.perform":
			performed.Add(1)
			var params map[string]any
			if json.Unmarshal(request.Params, &params) == nil {
				_, ok := params["__preflight"].(map[string]any)
				forwardedPreflight.Store(ok)
			}
			return map[string]any{"performed": true}, nil
		case "bridge.event":
			return map[string]any{"accepted": true}, nil
		default:
			return nil, protocol.NewError(protocol.CodeMethodNotFound, "METHOD_NOT_FOUND", "not implemented by test bridge", false, nil)
		}
	}
	trusted, err := rpc.Dial(context.Background(), "unix", cfg.BridgeSocketPath, bridgeHandler)
	if err != nil {
		t.Fatalf("dial action bridge: %v", err)
	}
	t.Cleanup(func() { _ = trusted.Close() })
	authenticateBridge(t, trusted, cfg)
	registerBridge(t, trusted, "browser-action")

	rawSession, rpcErr := call(t, public, "session.open", map[string]any{"name": "action", "clientId": "test"})
	if rpcErr != nil {
		t.Fatalf("session.open error = %v", rpcErr)
	}
	var session core.Session
	if err := json.Unmarshal(rawSession, &session); err != nil {
		t.Fatal(err)
	}
	rawClaim, rpcErr := call(t, public, "tab.claim", map[string]any{
		"sessionId": session.SessionID, "browserInstanceId": "browser-action", "tabId": "tab-action",
	})
	if rpcErr != nil {
		t.Fatalf("tab.claim error = %v", rpcErr)
	}
	var claim map[string]any
	if err := json.Unmarshal(rawClaim, &claim); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{
		"sessionId": session.SessionID, "browserInstanceId": "browser-action", "tabId": "tab-action",
		"leaseId": claim["leaseId"], "operationId": "op_action", "expectedDocumentEpoch": 1,
		"action": map[string]any{"type": "click", "target": map[string]any{"locator": map[string]any{"by": "role", "role": "button", "name": "Save"}}},
	}
	_, rpcErr = call(t, public, "action.perform", params)
	assertServerRPCError(t, rpcErr, protocol.CodeConfirmationNeeded, "CONFIRMATION_REQUIRED")
	var errorData map[string]any
	if err := json.Unmarshal(rpcErr.Data, &errorData); err != nil {
		t.Fatal(err)
	}
	confirmationID, _ := errorData["confirmationId"].(string)
	if confirmationID == "" || performed.Load() != 0 {
		t.Fatalf("first action confirmation = %q, performed = %d", confirmationID, performed.Load())
	}
	if _, rpcErr := call(t, trusted, "bridge.admin.confirmation.respond", map[string]any{"confirmationId": confirmationID, "decision": "approve"}); rpcErr != nil {
		t.Fatalf("approve action confirmation: %v", rpcErr)
	}
	params["confirmationId"] = confirmationID
	result, rpcErr := call(t, public, "action.perform", params)
	if rpcErr != nil {
		t.Fatalf("approved action error = %v", rpcErr)
	}
	if performed.Load() != 1 {
		t.Fatalf("performed calls = %d, want 1", performed.Load())
	}
	if !forwardedPreflight.Load() {
		t.Fatal("approved action did not receive the daemon-verified preflight result")
	}
	result, rpcErr = call(t, public, "action.perform", params)
	if rpcErr != nil || performed.Load() != 1 {
		t.Fatalf("action replay = (%s, %v), performed = %d", result, rpcErr, performed.Load())
	}
	if preflights.Load() != 2 {
		t.Fatalf("preflight calls = %d, want 2 (request and approved execution only)", preflights.Load())
	}
}

func TestForwardRejectsMissingLeaseFieldsBeforeSessionLookup(t *testing.T) {
	_, public, _, _ := startTestServer(t)
	_, rpcErr := call(t, public, "tab.activate", map[string]any{"tabId": "tab-missing-lease"})
	assertServerRPCError(t, rpcErr, protocol.CodeInvalidParams, "INVALID_REQUEST")
	if !strings.Contains(rpcErr.Message, "sessionId") || !strings.Contains(rpcErr.Message, "leaseId") {
		t.Fatalf("missing lease error = %q", rpcErr.Message)
	}
}

func TestActionPreflightTimeoutHasNoPossibleEffect(t *testing.T) {
	_, public, _, cfg := startTestServer(t)
	var performed atomic.Int32
	bridgeHandler := func(_ context.Context, _ *rpc.Peer, request protocol.Request) (any, *protocol.RPCError) {
		switch request.Method {
		case "tab.claim":
			return map[string]any{"tabId": "tab-preflight-timeout", "documentEpoch": 1, "claimed": true}, nil
		case "action.preflight":
			return nil, protocol.NewError(protocol.CodeBridgeTimeout, "TIMEOUT", "preflight timed out", true, map[string]any{"method": "action.preflight"})
		case "action.perform":
			performed.Add(1)
			return map[string]any{"performed": true}, nil
		default:
			return nil, protocol.NewError(protocol.CodeMethodNotFound, "METHOD_NOT_FOUND", "not implemented by test bridge", false, nil)
		}
	}
	trusted, err := rpc.Dial(context.Background(), "unix", cfg.BridgeSocketPath, bridgeHandler)
	if err != nil {
		t.Fatalf("dial timeout bridge: %v", err)
	}
	t.Cleanup(func() { _ = trusted.Close() })
	authenticateBridge(t, trusted, cfg)
	registerBridge(t, trusted, "browser-preflight-timeout")

	rawSession, rpcErr := call(t, public, "session.open", map[string]any{"name": "preflight-timeout"})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var session core.Session
	if err := json.Unmarshal(rawSession, &session); err != nil {
		t.Fatal(err)
	}
	rawClaim, rpcErr := call(t, public, "tab.claim", map[string]any{
		"sessionId": session.SessionID, "browserInstanceId": "browser-preflight-timeout", "tabId": "tab-preflight-timeout",
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var claim map[string]any
	if err := json.Unmarshal(rawClaim, &claim); err != nil {
		t.Fatal(err)
	}
	_, rpcErr = call(t, public, "action.perform", map[string]any{
		"sessionId": session.SessionID, "tabId": "tab-preflight-timeout", "leaseId": claim["leaseId"],
		"operationId": "op_preflight_timeout", "expectedDocumentEpoch": 1,
		"action": map[string]any{"type": "press", "key": "PageDown"},
	})
	assertServerRPCError(t, rpcErr, protocol.CodeBridgeTimeout, "TIMEOUT")
	var data map[string]any
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["effect"] != "none" || performed.Load() != 0 {
		t.Fatalf("preflight timeout data = %v, performed = %d", data, performed.Load())
	}
}

func TestArtifactsAreSessionScopedAndLocalPathNeedsCapability(t *testing.T) {
	server, public, _, _ := startTestServer(t)
	owner := server.core.OpenSession("artifact-owner", "test")
	other := server.core.OpenSession("artifact-other", "test")
	artifact, rpcErr := server.core.StoreArtifact(owner.SessionID, "tab-artifact", "test", "text/plain", "result.txt", []byte("artifact body"), time.Minute)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}

	_, rpcErr = call(t, public, "artifact.get", map[string]any{"artifactId": artifact.ArtifactID})
	assertServerRPCError(t, rpcErr, protocol.CodeInvalidParams, "INVALID_REQUEST")
	_, rpcErr = call(t, public, "artifact.get", map[string]any{"sessionId": other.SessionID, "artifactId": artifact.ArtifactID})
	assertServerRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")

	raw, rpcErr := call(t, public, "artifact.get", map[string]any{"sessionId": owner.SessionID, "artifactId": artifact.ArtifactID})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if strings.Contains(string(raw), "localPath") {
		t.Fatalf("artifact metadata exposed local path without capability: %s", raw)
	}
	_, rpcErr = call(t, public, "artifact.get", map[string]any{"sessionId": owner.SessionID, "artifactId": artifact.ArtifactID, "includeLocalPath": true})
	assertServerRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "CAPABILITY_REQUIRED")
	confirmation, rpcErr := server.core.CreateCapabilityConfirmation(owner.SessionID, "browser-artifact", []string{"artifact.localPath"}, time.Minute)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if _, rpcErr := server.core.ResolveConfirmation(confirmation.ConfirmationID, "approve"); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	raw, rpcErr = call(t, public, "artifact.get", map[string]any{"sessionId": owner.SessionID, "artifactId": artifact.ArtifactID, "includeLocalPath": true})
	if rpcErr != nil || !strings.Contains(string(raw), "localPath") {
		t.Fatalf("artifact local path = (%s, %v)", raw, rpcErr)
	}

	_, rpcErr = call(t, public, "artifact.readChunk", map[string]any{"sessionId": other.SessionID, "artifactId": artifact.ArtifactID})
	assertServerRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")
	raw, rpcErr = call(t, public, "artifact.readChunk", map[string]any{"sessionId": owner.SessionID, "artifactId": artifact.ArtifactID})
	if rpcErr != nil || !strings.Contains(string(raw), "YXJ0aWZhY3QgYm9keQ==") {
		t.Fatalf("artifact read = (%s, %v)", raw, rpcErr)
	}
	_, rpcErr = call(t, public, "artifact.delete", map[string]any{"sessionId": other.SessionID, "artifactId": artifact.ArtifactID})
	assertServerRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")
	if _, rpcErr := call(t, public, "artifact.delete", map[string]any{"sessionId": owner.SessionID, "artifactId": artifact.ArtifactID}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
}

func TestSessionIDIsNonEnumerableBearerForScopedState(t *testing.T) {
	server, public, _, _ := startTestServer(t)
	owner := server.core.OpenSession("scope-owner", "test")
	other := server.core.OpenSession("scope-other", "test")

	_, rpcErr := call(t, public, "session.get", map[string]any{})
	assertServerRPCError(t, rpcErr, protocol.CodeInvalidParams, "INVALID_REQUEST")

	confirmation, rpcErr := server.core.CreateCapabilityConfirmation(owner.SessionID, "browser-owner", []string{"clipboard.read"}, time.Minute)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	_, rpcErr = call(t, public, "confirmation.get", map[string]any{
		"sessionId": other.SessionID, "confirmationId": confirmation.ConfirmationID,
	})
	assertServerRPCError(t, rpcErr, protocol.CodeCapabilityDenied, "PERMISSION_DENIED")

	server.core.EmitEvent(core.Event{Type: "owner-only", SessionID: owner.SessionID})
	server.core.EmitEvent(core.Event{Type: "other-only", SessionID: other.SessionID})
	raw, rpcErr := call(t, public, "event.replay", map[string]any{"sessionId": owner.SessionID})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !strings.Contains(string(raw), "owner-only") || strings.Contains(string(raw), "other-only") {
		t.Fatalf("session-filtered events = %s", raw)
	}
	_, rpcErr = call(t, public, "event.replay", map[string]any{})
	assertServerRPCError(t, rpcErr, protocol.CodeInvalidParams, "INVALID_REQUEST")

	_, rpcErr, _ = server.core.ExecuteOperation(context.Background(), owner.SessionID, "tab-1", "op-scope", map[string]any{"kind": "read"}, func(context.Context) (json.RawMessage, *protocol.RPCError) {
		return json.RawMessage(`{"ok":true}`), nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	_, rpcErr = call(t, public, "operation.get", map[string]any{"sessionId": other.SessionID, "operationId": "op-scope"})
	assertServerRPCError(t, rpcErr, protocol.CodeInvalidState, "INVALID_REQUEST")
}

func TestDownloadExpectationRequiresDownloadCapability(t *testing.T) {
	params := map[string]any{
		"action": map[string]any{"type": "click"},
		"expect": []any{map[string]any{"type": "download"}},
	}
	if got := capabilityForRequest("action.perform", params); got != "files.download" {
		t.Fatalf("capabilityForRequest() = %q, want files.download", got)
	}
}

func startTestServer(t *testing.T) (*Server, *rpc.Peer, *rpc.Peer, config.Config) {
	t.Helper()
	// Darwin limits Unix-domain socket paths to roughly 104 bytes; t.TempDir()
	// includes the full test name and can exceed that limit.
	root, err := os.MkdirTemp("/tmp", "browser-control-test-")
	if err != nil {
		t.Fatalf("create short temporary directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cfg := config.Config{
		StateDir:         root,
		SocketPath:       filepath.Join(root, "public.sock"),
		BridgeSocketPath: filepath.Join(root, "bridge.sock"),
		BridgeTokenPath:  filepath.Join(root, "bridge.token"),
		ArtifactDir:      filepath.Join(root, "artifacts"),
		UploadRoots:      []string{root},
		LeaseTTL:         time.Minute,
		EventLimit:       256,
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()

	public, err := rpc.Dial(context.Background(), "unix", cfg.SocketPath, nil)
	if err != nil {
		cancel()
		_ = server.Close()
		t.Fatalf("dial public socket: %v", err)
	}
	trusted, err := rpc.Dial(context.Background(), "unix", cfg.BridgeSocketPath, nil)
	if err != nil {
		_ = public.Close()
		cancel()
		_ = server.Close()
		t.Fatalf("dial bridge socket: %v", err)
	}
	authenticateBridge(t, trusted, cfg)

	t.Cleanup(func() {
		_ = public.Close()
		_ = trusted.Close()
		cancel()
		_ = server.Close()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("Serve() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Error("timed out waiting for server shutdown")
		}
		server.Wait()
	})
	return server, public, trusted, cfg
}

func registerBridge(t *testing.T, peer *rpc.Peer, browserInstanceID string) {
	t.Helper()
	_, rpcErr := call(t, peer, "bridge.hello", testBridgeHello(browserInstanceID))
	if rpcErr != nil {
		t.Fatalf("bridge.hello(%s) error = %v", browserInstanceID, rpcErr)
	}
}

func testBridgeHello(browserInstanceID string) map[string]any {
	return map[string]any{
		"protocolVersion":   protocol.Version,
		"browserInstanceId": browserInstanceID,
		"profileName":       "test",
		"chromeVersion":     "test",
		"extensionVersion":  "test",
	}
}

func authenticateBridge(t *testing.T, peer *rpc.Peer, cfg config.Config) {
	t.Helper()
	token, err := bridgeauth.LoadToken(cfg.BridgeTokenPath)
	if err != nil {
		t.Fatalf("load bridge token: %v", err)
	}
	_, rpcErr := call(t, peer, bridgeauth.Method, map[string]any{"token": bridgeauth.EncodedToken(token)})
	if rpcErr != nil {
		t.Fatalf("authenticate bridge transport: %v", rpcErr)
	}
}

func call(t *testing.T, peer *rpc.Peer, method string, params any) (json.RawMessage, *protocol.RPCError) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, rpcErr, err := peer.Call(ctx, method, params)
	if err != nil {
		t.Fatalf("Call(%s) transport error = %v", method, err)
	}
	return result, rpcErr
}

func assertServerRPCError(t *testing.T, got *protocol.RPCError, code int, kind string) {
	t.Helper()
	if got == nil {
		t.Fatalf("RPC error = nil, want code %d kind %q", code, kind)
	}
	if got.Code != code || protocol.ErrorKind(got) != kind {
		t.Fatalf("RPC error = %+v (kind %q), want code %d kind %q", got, protocol.ErrorKind(got), code, kind)
	}
}
