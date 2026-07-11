package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cc-hchi/browser-control/internal/bridgeauth"
	"github.com/cc-hchi/browser-control/internal/config"
	"github.com/cc-hchi/browser-control/internal/protocol"
)

func TestParseArgsAndValidObject(t *testing.T) {
	opts, err := parseArgs([]string{"--json", "--timeout", "2s", "rpc", "tab.list", "--params", `{}`}, "/tmp/default.sock")
	if err != nil || !opts.json || opts.timeout != 2*time.Second || opts.command != "rpc" || len(opts.args) != 3 {
		t.Fatalf("parseArgs() = (%+v, %v)", opts, err)
	}
	if !validObject([]byte(`{"a":1}`)) || validObject([]byte(`[]`)) || validObject([]byte(`{} {}`)) {
		t.Fatal("validObject did not enforce exactly one JSON object")
	}
}

func TestDoctorChecksInstallAndExtensionRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state, err := os.MkdirTemp("/tmp", "browser-control-doctor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	artifacts := filepath.Join(state, "artifacts")
	binDir := filepath.Join(state, "bin")
	extensionDir := filepath.Join(state, "extension")
	for _, dir := range []string{state, artifacts, binDir, extensionDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	host := filepath.Join(binDir, "browser-native-host")
	if err := os.WriteFile(host, []byte("host"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extensionDir, "manifest.json"), []byte(`{"manifest_version":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(state, "bridge.token")
	if err := os.WriteFile(tokenPath, make([]byte, bridgeauth.TokenBytes), 0o600); err != nil {
		t.Fatal(err)
	}

	bridgePath := filepath.Join(state, "bridge.sock")
	bridgeListener, err := net.Listen("unix", bridgePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridgeListener.Close() })
	if err := os.Chmod(bridgePath, 0o600); err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(state, "browserd.sock")
	publicListener, err := net.Listen("unix", publicPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publicListener.Close() })
	if err := os.Chmod(publicPath, 0o600); err != nil {
		t.Fatal(err)
	}

	nativeDir := filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts")
	if err := os.MkdirAll(nativeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeManifest := map[string]any{
		"name": "com.browser_control.native_host", "path": host, "type": "stdio",
		"allowed_origins": []string{bridgeauth.StableExtensionOrigin},
	}
	nativeRaw, _ := json.Marshal(nativeManifest)
	if err := os.WriteFile(filepath.Join(nativeDir, "com.browser_control.native_host.json"), nativeRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	serveDone := make(chan error, 1)
	go func() {
		for requestNumber := 0; requestNumber < 2; requestNumber++ {
			conn, err := publicListener.Accept()
			if err != nil {
				serveDone <- err
				return
			}
			var request protocol.Request
			err = json.NewDecoder(conn).Decode(&request)
			if err == nil {
				var result any
				switch request.Method {
				case "daemon.diagnostics":
					result = map[string]any{
						"daemon":       map[string]any{"protocolVersion": protocol.Version},
						"bridgeSocket": map[string]any{"path": bridgePath},
						"browsers":     []any{map[string]any{"browserInstanceId": "browser-test"}},
					}
				case "browser.list":
					result = map[string]any{"browsers": []any{map[string]any{"browserId": "browser-test"}}}
				}
				err = json.NewEncoder(conn).Encode(protocol.Response{JSONRPC: "2.0", ID: request.ID, Result: protocol.MarshalResult(result)})
			}
			_ = conn.Close()
			if err != nil {
				serveDone <- err
				return
			}
		}
		serveDone <- nil
	}()

	cfg := config.Config{
		StateDir: state, SocketPath: publicPath, BridgeSocketPath: bridgePath, BridgeTokenPath: tokenPath,
		ArtifactDir: artifacts, UploadRoots: []string{home}, LeaseTTL: time.Minute, EventLimit: 10,
	}
	var output bytes.Buffer
	if code := runDoctor(options{socket: publicPath, timeout: time.Second, command: "doctor", json: true}, cfg, &output); code != 0 {
		t.Fatalf("runDoctor() code = %d, output = %s", code, output.String())
	}
	var result map[string]any
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result["ok"] != true {
		t.Fatalf("doctor output = %s, error = %v", output.String(), err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for doctor test server")
	}
}
