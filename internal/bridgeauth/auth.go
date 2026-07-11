// Package bridgeauth authenticates the Native Messaging transport before the
// extension-facing JSON-RPC bridge is exposed. The token never travels over the
// public browserd socket and is not included in diagnostics.
package bridgeauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/cc-hchi/browser-control/internal/protocol"
)

const (
	Method                = "bridge.transport.authenticate"
	TokenBytes            = 32
	StableExtensionOrigin = "chrome-extension://bfnlcmlokggpcgncalophomjeencbbli/"
)

// LoadOrCreateToken loads a persistent token or creates it with O_EXCL. Only
// browserd calls this function; the native host is intentionally read-only.
func LoadOrCreateToken(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("bridge token path must be absolute")
	}
	token := make([]byte, TokenBytes)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generate bridge transport token: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return LoadToken(path)
	}
	if err != nil {
		return nil, fmt.Errorf("create bridge transport token: %w", err)
	}
	created := true
	defer func() {
		_ = file.Close()
		if created {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(token); err != nil {
		return nil, fmt.Errorf("write bridge transport token: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync bridge transport token: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close bridge transport token: %w", err)
	}
	created = false
	return token, nil
}

// LoadToken reads an existing token and rejects symlinks, non-regular files,
// permissive modes, and malformed lengths.
func LoadToken(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("bridge token path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect bridge transport token: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("bridge transport token must be a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("bridge transport token mode must be 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open bridge transport token: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened bridge transport token: %w", err)
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("bridge transport token changed while opening")
	}
	token, err := io.ReadAll(io.LimitReader(file, TokenBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read bridge transport token: %w", err)
	}
	if len(token) != TokenBytes {
		return nil, fmt.Errorf("bridge transport token must contain exactly %d bytes", TokenBytes)
	}
	return token, nil
}

func EncodedToken(token []byte) string {
	return base64.RawStdEncoding.EncodeToString(token)
}

// Matches compares fixed-size hashes so malformed or differently-sized input
// does not create a token-length timing oracle.
func Matches(expected []byte, provided string) bool {
	expectedHash := sha256.Sum256([]byte(EncodedToken(expected)))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

// Authenticate performs the one-shot daemon-side transport authentication
// request before any Chrome messages are relayed.
func Authenticate(conn net.Conn, token []byte, timeout time.Duration) error {
	if len(token) != TokenBytes {
		return fmt.Errorf("invalid bridge transport token")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set bridge authentication deadline: %w", err)
	}
	defer conn.SetDeadline(time.Time{})
	request := protocol.Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"native-host-auth"`),
		Method:  Method,
		Params:  protocol.MarshalResult(map[string]any{"token": EncodedToken(token)}),
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return fmt.Errorf("send bridge transport authentication: %w", err)
	}
	var response protocol.Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return fmt.Errorf("read bridge transport authentication: %w", err)
	}
	if response.JSONRPC != "2.0" || protocol.IDKey(response.ID) != `"native-host-auth"` {
		return fmt.Errorf("invalid bridge transport authentication response")
	}
	if response.Error != nil {
		return fmt.Errorf("bridge transport authentication rejected")
	}
	var result struct {
		Accepted bool `json:"accepted"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil || !result.Accepted {
		return fmt.Errorf("bridge transport authentication was not accepted")
	}
	return nil
}
