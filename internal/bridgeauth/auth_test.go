package bridgeauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cc-hchi/browser-control/internal/protocol"
)

func TestLoadOrCreateTokenIsPersistentAndMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.token")
	first, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("LoadOrCreateToken(first): %v", err)
	}
	if len(first) != TokenBytes {
		t.Fatalf("token length = %d, want %d", len(first), TokenBytes)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %04o, want 0600", info.Mode().Perm())
	}
	second, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("LoadOrCreateToken(second): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("existing token was replaced")
	}
}

func TestLoadTokenRejectsUnsafeOrMalformedFiles(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name  string
		setup func(string) error
		want  string
	}{
		{name: "wrong mode", setup: func(path string) error {
			if err := os.WriteFile(path, make([]byte, TokenBytes), 0o600); err != nil {
				return err
			}
			return os.Chmod(path, 0o644)
		}, want: "mode must be 0600"},
		{name: "wrong length", setup: func(path string) error { return os.WriteFile(path, []byte("short"), 0o600) }, want: "exactly 32 bytes"},
		{name: "symlink", setup: func(path string) error {
			target := filepath.Join(root, "target")
			if err := os.WriteFile(target, make([]byte, TokenBytes), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}, want: "regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(root, strings.ReplaceAll(tt.name, " ", "-"))
			if err := tt.setup(path); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadToken(path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadToken() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestMatchesUsesEncodedToken(t *testing.T) {
	token := bytes.Repeat([]byte{0x5a}, TokenBytes)
	if !Matches(token, EncodedToken(token)) {
		t.Fatal("correct token did not match")
	}
	if Matches(token, EncodedToken(bytes.Repeat([]byte{0x5b}, TokenBytes))) || Matches(token, "") {
		t.Fatal("incorrect token matched")
	}
}

func TestStableExtensionOriginMatchesManifestKey(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "apps", "extension", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(manifest.Key)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(key)
	const alphabet = "abcdefghijklmnop"
	id := make([]byte, 0, 32)
	for _, value := range digest[:16] {
		id = append(id, alphabet[value>>4], alphabet[value&0x0f])
	}
	if got := "chrome-extension://" + string(id) + "/"; got != StableExtensionOrigin {
		t.Fatalf("manifest origin = %q, native host allows %q", got, StableExtensionOrigin)
	}
}

func TestAuthenticateHandshake(t *testing.T) {
	token := bytes.Repeat([]byte{0x7c}, TokenBytes)
	client, daemon := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = daemon.Close()
	})
	daemonDone := make(chan error, 1)
	go func() {
		var request protocol.Request
		if err := json.NewDecoder(daemon).Decode(&request); err != nil {
			daemonDone <- err
			return
		}
		if request.Method != Method {
			daemonDone <- &unexpectedMethodError{got: request.Method}
			return
		}
		var params map[string]any
		if err := json.Unmarshal(request.Params, &params); err != nil {
			daemonDone <- err
			return
		}
		if params["token"] != EncodedToken(token) {
			daemonDone <- &unexpectedTokenError{}
			return
		}
		response := protocol.Response{JSONRPC: "2.0", ID: request.ID, Result: protocol.MarshalResult(map[string]any{"accepted": true})}
		daemonDone <- json.NewEncoder(daemon).Encode(response)
	}()
	if err := Authenticate(client, token, time.Second); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if err := <-daemonDone; err != nil {
		t.Fatalf("daemon handshake error = %v", err)
	}
}

type unexpectedMethodError struct{ got string }

func (e *unexpectedMethodError) Error() string { return "unexpected method: " + e.got }

type unexpectedTokenError struct{}

func (*unexpectedTokenError) Error() string { return "unexpected token" }
