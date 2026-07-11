package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	EnvSocket       = "BROWSER_CONTROL_SOCKET"
	EnvBridgeSocket = "BROWSER_CONTROL_BRIDGE_SOCKET"
	EnvBridgeToken  = "BROWSER_CONTROL_BRIDGE_TOKEN_FILE"
	EnvStateDir     = "BROWSER_CONTROL_STATE_DIR"
	EnvArtifactDir  = "BROWSER_CONTROL_ARTIFACT_DIR"
	EnvNativeHost   = "BROWSER_CONTROL_NATIVE_MANIFEST"
	EnvUploadRoots  = "BROWSER_CONTROL_UPLOAD_ROOTS"
	DefaultLeaseTTL = 2 * time.Minute
)

type Config struct {
	StateDir         string
	SocketPath       string
	BridgeSocketPath string
	BridgeTokenPath  string
	ArtifactDir      string
	NativeManifest   string
	UploadRoots      []string
	LeaseTTL         time.Duration
	EventLimit       int
}

func Default() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home directory: %w", err)
	}
	stateDir := os.Getenv(EnvStateDir)
	if stateDir == "" {
		stateDir = filepath.Join(home, "Library", "Application Support", "browser-control")
	}
	socket := os.Getenv(EnvSocket)
	if socket == "" {
		socket = filepath.Join(stateDir, "browserd.sock")
	}
	bridgeSocket := os.Getenv(EnvBridgeSocket)
	if bridgeSocket == "" {
		bridgeSocket = filepath.Join(stateDir, "bridge.sock")
	}
	bridgeToken := os.Getenv(EnvBridgeToken)
	if bridgeToken == "" {
		bridgeToken = filepath.Join(stateDir, "bridge.token")
	}
	artifacts := os.Getenv(EnvArtifactDir)
	if artifacts == "" {
		artifacts = filepath.Join(stateDir, "artifacts")
	}
	nativeManifest := os.Getenv(EnvNativeHost)
	if nativeManifest == "" {
		nativeManifest = filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts", "com.browser_control.native_host.json")
	}
	uploadRoots := []string{
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "Desktop"),
	}
	// A non-empty environment setting intentionally replaces the defaults. The
	// value uses the platform path-list separator (':' on macOS).
	if raw := os.Getenv(EnvUploadRoots); raw != "" {
		uploadRoots = filepath.SplitList(raw)
	}
	cfg := Config{
		StateDir:         stateDir,
		SocketPath:       socket,
		BridgeSocketPath: bridgeSocket,
		BridgeTokenPath:  bridgeToken,
		ArtifactDir:      artifacts,
		NativeManifest:   nativeManifest,
		UploadRoots:      uploadRoots,
		LeaseTTL:         DefaultLeaseTTL,
		EventLimit:       1024,
	}
	// Validation is deferred until Server.New so an explicit --upload-root can
	// still override a malformed environment setting.
	return cfg, nil
}

// Normalized returns a copy with clean, unique upload roots. Upload roots are
// configuration boundaries, so they must be absolute, but they need not exist
// when browserd starts (for example, a user's Downloads folder may be created
// later).
func (c Config) Normalized() (Config, error) {
	if len(c.UploadRoots) == 0 {
		return Config{}, fmt.Errorf("at least one upload root is required")
	}
	seen := make(map[string]struct{}, len(c.UploadRoots))
	roots := make([]string, 0, len(c.UploadRoots))
	for _, root := range c.UploadRoots {
		if strings.TrimSpace(root) == "" {
			return Config{}, fmt.Errorf("upload roots must not be empty")
		}
		if !filepath.IsAbs(root) {
			return Config{}, fmt.Errorf("upload root must be absolute: %s", root)
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	c.UploadRoots = roots
	return c, nil
}

func (c Config) Prepare() error {
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(c.StateDir, 0o700); err != nil {
		return fmt.Errorf("secure state directory: %w", err)
	}
	if err := os.MkdirAll(c.ArtifactDir, 0o700); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	if err := os.Chmod(c.ArtifactDir, 0o700); err != nil {
		return fmt.Errorf("secure artifact directory: %w", err)
	}
	return nil
}
