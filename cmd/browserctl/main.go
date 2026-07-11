package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cc-hchi/browser-control/internal/bridgeauth"
	"github.com/cc-hchi/browser-control/internal/config"
	"github.com/cc-hchi/browser-control/internal/protocol"
)

const maxCLIMessage = 128 * 1024 * 1024

type options struct {
	socket  string
	timeout time.Duration
	json    bool
	command string
	args    []string
}

type cliFailure struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout))
}

func run(args []string, stdin io.Reader, stdout io.Writer) int {
	cfg, err := config.Default()
	if err != nil {
		writeFailure(stdout, "CONFIG_ERROR", "Could not resolve browser-control configuration.", "Set HOME and retry.")
		return 1
	}
	parsed, err := parseArgs(args, cfg.SocketPath)
	if err != nil {
		writeFailure(stdout, "INVALID_ARGUMENT", err.Error(), "Use: browserctl --json doctor, or browserctl --json rpc METHOD --params JSON")
		return 2
	}
	switch parsed.command {
	case "doctor":
		return runDoctor(parsed, cfg, stdout)
	case "rpc":
		return runRPC(parsed, stdin, stdout)
	default:
		writeFailure(stdout, "INVALID_ARGUMENT", "A command is required.", "Use doctor or rpc.")
		return 2
	}
}

func parseArgs(args []string, defaultSocket string) (options, error) {
	result := options{socket: defaultSocket, timeout: 30 * time.Second}
	for len(args) > 0 {
		switch args[0] {
		case "--json":
			result.json = true
			args = args[1:]
		case "--socket":
			if len(args) < 2 || args[1] == "" {
				return options{}, fmt.Errorf("--socket requires a path")
			}
			result.socket = args[1]
			args = args[2:]
		case "--timeout":
			if len(args) < 2 {
				return options{}, fmt.Errorf("--timeout requires a duration")
			}
			timeout, err := time.ParseDuration(args[1])
			if err != nil || timeout <= 0 || timeout > 10*time.Minute {
				return options{}, fmt.Errorf("--timeout must be between 1ns and 10m")
			}
			result.timeout = timeout
			args = args[2:]
		default:
			if strings.HasPrefix(args[0], "-") {
				return options{}, fmt.Errorf("unknown global option %s", args[0])
			}
			result.command = args[0]
			result.args = append([]string(nil), args[1:]...)
			return result, nil
		}
	}
	return result, nil
}

func runRPC(opts options, stdin io.Reader, stdout io.Writer) int {
	if len(opts.args) == 0 || opts.args[0] == "" || strings.HasPrefix(opts.args[0], "-") {
		writeFailure(stdout, "INVALID_ARGUMENT", "rpc requires a method.", "Use: browserctl --json rpc METHOD --params JSON")
		return 2
	}
	method := opts.args[0]
	paramsText := "{}"
	for index := 1; index < len(opts.args); index++ {
		argument := opts.args[index]
		if argument == "--params" {
			if index+1 >= len(opts.args) {
				writeFailure(stdout, "INVALID_ARGUMENT", "--params requires JSON or -.", "Pass a JSON object, or use --params - to read stdin.")
				return 2
			}
			paramsText = opts.args[index+1]
			index++
			continue
		}
		if strings.HasPrefix(argument, "--params=") {
			paramsText = strings.TrimPrefix(argument, "--params=")
			continue
		}
		writeFailure(stdout, "INVALID_ARGUMENT", "Unknown rpc option: "+argument, "Use only --params JSON after METHOD.")
		return 2
	}
	var params []byte
	if paramsText == "-" {
		limited := io.LimitReader(stdin, maxCLIMessage+1)
		params, _ = io.ReadAll(limited)
		if len(params) > maxCLIMessage {
			writeFailure(stdout, "INVALID_ARGUMENT", "stdin parameters exceed the 128 MiB limit.", "Pass a smaller request or store large data as an artifact.")
			return 2
		}
	} else {
		params = []byte(paramsText)
	}
	if !validObject(params) {
		writeFailure(stdout, "INVALID_ARGUMENT", "--params must be one valid JSON object.", "Correct the JSON without printing sensitive values.")
		return 2
	}

	response, err := call(opts.socket, opts.timeout, method, params)
	if err != nil {
		writeFailure(stdout, "DAEMON_UNAVAILABLE", "Could not complete the local browserd request.", daemonRemediation(opts.socket))
		return 1
	}
	_, _ = stdout.Write(append(response, '\n'))
	var envelope protocol.Response
	if json.Unmarshal(response, &envelope) != nil || envelope.Error != nil {
		return 1
	}
	return 0
}

func runDoctor(opts options, cfg config.Config, stdout io.Writer) int {
	if len(opts.args) != 0 {
		writeFailure(stdout, "INVALID_ARGUMENT", "doctor does not accept positional arguments.", "Run browserctl --json doctor.")
		return 2
	}
	response, err := call(opts.socket, opts.timeout, "daemon.diagnostics", []byte("{}"))
	if err != nil {
		writeFailure(stdout, "DAEMON_UNAVAILABLE", "browserd is not reachable.", daemonRemediation(opts.socket))
		return 1
	}
	var envelope protocol.Response
	if json.Unmarshal(response, &envelope) != nil || envelope.Error != nil {
		writeFailure(stdout, "DAEMON_ERROR", "browserd diagnostics failed.", "Inspect browserd stderr and restart its LaunchAgent.")
		return 1
	}
	var diagnostics map[string]any
	if json.Unmarshal(envelope.Result, &diagnostics) != nil {
		writeFailure(stdout, "PROTOCOL_ERROR", "browserd returned invalid diagnostics.", "Reinstall matching browser-control binaries.")
		return 1
	}
	protocolVersion := nestedString(diagnostics, "daemon", "protocolVersion")
	browsers, _ := diagnostics["browsers"].([]any)
	if protocolVersion != protocol.Version {
		writeDoctor(stdout, false, "PROTOCOL_MISMATCH", "browserctl and browserd protocol versions differ.", "Reinstall browser-control so every component is from the same build.", diagnostics)
		return 1
	}
	checks := localDoctorChecks(cfg, opts.socket, diagnostics)
	diagnostics["localChecks"] = checks
	for _, check := range checks {
		if ok, _ := check["ok"].(bool); !ok {
			writeDoctor(stdout, false, "INSTALLATION_INVALID", "browser-control installation metadata or permissions are invalid.", "Rerun scripts/install.sh, reload the unpacked extension, and retry doctor.", diagnostics)
			return 1
		}
	}
	if len(browsers) == 0 {
		writeDoctor(stdout, false, "EXTENSION_DISCONNECTED", "browserd is running, but no Chrome extension is connected.", "Load or reload the installed Local Chrome Control extension, then retry.", diagnostics)
		return 1
	}
	roundTrip, roundTripErr := call(opts.socket, opts.timeout, "browser.list", []byte("{}"))
	if roundTripErr != nil {
		writeDoctor(stdout, false, "EXTENSION_UNRESPONSIVE", "The extension is registered but did not complete a round-trip request.", "Reload Local Chrome Control in chrome://extensions and retry.", diagnostics)
		return 1
	}
	var roundTripEnvelope protocol.Response
	var browserList map[string]any
	if json.Unmarshal(roundTrip, &roundTripEnvelope) != nil || roundTripEnvelope.Error != nil || json.Unmarshal(roundTripEnvelope.Result, &browserList) != nil {
		writeDoctor(stdout, false, "EXTENSION_UNRESPONSIVE", "The extension round-trip returned an invalid response.", "Reload Local Chrome Control and reinstall matching binaries if the problem persists.", diagnostics)
		return 1
	}
	roundTripBrowsers, _ := browserList["browsers"].([]any)
	if len(roundTripBrowsers) == 0 {
		writeDoctor(stdout, false, "EXTENSION_UNRESPONSIVE", "The extension round-trip returned no Chrome instance.", "Reload Local Chrome Control in the intended Chrome profile.", diagnostics)
		return 1
	}
	diagnostics["extensionRoundTrip"] = map[string]any{"ok": true, "browserCount": len(roundTripBrowsers)}
	writeDoctor(stdout, true, "", "browserd, Native Messaging, and the Chrome extension are connected.", "", diagnostics)
	return 0
}

func localDoctorChecks(cfg config.Config, publicSocket string, diagnostics map[string]any) []map[string]any {
	appDir := cfg.StateDir
	bridgeSocket := nestedString(diagnostics, "bridgeSocket", "path")
	if bridgeSocket == "" {
		bridgeSocket = cfg.BridgeSocketPath
	}
	nativeManifest := cfg.NativeManifest
	if nativeManifest == "" {
		home, _ := os.UserHomeDir()
		nativeManifest = filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts", "com.browser_control.native_host.json")
	}
	extensionManifest := filepath.Join(appDir, "extension", "manifest.json")
	hostBinary := filepath.Join(appDir, "bin", "browser-native-host")
	checks := []map[string]any{
		pathCheck("stateDirectory", appDir, 0o700, true, false),
		pathCheck("artifactDirectory", cfg.ArtifactDir, 0o700, true, false),
		pathCheck("publicSocket", publicSocket, 0o600, false, true),
		pathCheck("bridgeSocket", bridgeSocket, 0o600, false, true),
		pathCheck("bridgeToken", cfg.BridgeTokenPath, 0o600, false, false),
		pathCheck("nativeHost", hostBinary, 0o700, false, false),
		pathCheck("nativeManifest", nativeManifest, 0o600, false, false),
		pathCheck("extensionManifest", extensionManifest, 0o600, false, false),
	}
	var manifest struct {
		Name           string   `json:"name"`
		Path           string   `json:"path"`
		Type           string   `json:"type"`
		AllowedOrigins []string `json:"allowed_origins"`
	}
	raw, err := os.ReadFile(nativeManifest)
	manifestOK := err == nil && json.Unmarshal(raw, &manifest) == nil && manifest.Name == "com.browser_control.native_host" &&
		manifest.Type == "stdio" && filepath.Clean(manifest.Path) == filepath.Clean(hostBinary) && len(manifest.AllowedOrigins) == 1 &&
		manifest.AllowedOrigins[0] == bridgeauth.StableExtensionOrigin
	checks = append(checks, map[string]any{"name": "nativeManifestContents", "ok": manifestOK})
	return checks
}

func pathCheck(name, path string, expectedMode os.FileMode, wantDirectory, wantSocket bool) map[string]any {
	result := map[string]any{"name": name, "ok": false}
	info, err := os.Lstat(path)
	if err != nil {
		result["reason"] = "missing"
		return result
	}
	if info.Mode()&os.ModeSymlink != 0 {
		result["reason"] = "symlink"
		return result
	}
	if info.Mode().Perm() != expectedMode {
		result["reason"] = "wrong-mode"
		result["actualMode"] = fmt.Sprintf("%04o", info.Mode().Perm())
		return result
	}
	if wantDirectory && !info.IsDir() {
		result["reason"] = "not-directory"
		return result
	}
	if wantSocket && info.Mode()&os.ModeSocket == 0 {
		result["reason"] = "not-socket"
		return result
	}
	if !wantDirectory && !wantSocket && !info.Mode().IsRegular() {
		result["reason"] = "not-regular"
		return result
	}
	result["ok"] = true
	return result
}

func call(socket string, timeout time.Duration, method string, params json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	request := protocol.Request{JSONRPC: "2.0", ID: protocol.MarshalResult("cli-" + hex.EncodeToString(idBytes)), Method: method, Params: params}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(io.LimitReader(conn, maxCLIMessage+1))
	var response json.RawMessage
	if err := json.NewDecoder(reader).Decode(&response); err != nil {
		return nil, err
	}
	if len(response) > maxCLIMessage || !json.Valid(response) {
		return nil, errors.New("invalid or oversized daemon response")
	}
	return bytes.TrimSpace(response), nil
}

func validObject(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || value == nil {
		return false
	}
	var trailing any
	return decoder.Decode(&trailing) == io.EOF
}

func nestedString(values map[string]any, object, key string) string {
	nested, _ := values[object].(map[string]any)
	value, _ := nested[key].(string)
	return value
}

func daemonRemediation(socket string) string {
	return "Start or reinstall browserd and verify its private Unix socket at " + socket + "."
}

func writeFailure(out io.Writer, code, message, remediation string) {
	_ = json.NewEncoder(out).Encode(map[string]any{"ok": false, "error": cliFailure{Code: code, Message: message, Remediation: remediation}})
}

func writeDoctor(out io.Writer, ok bool, code, message, remediation string, diagnostics map[string]any) {
	result := map[string]any{"ok": ok, "message": message, "diagnostics": diagnostics}
	if !ok {
		result["error"] = cliFailure{Code: code, Message: message, Remediation: remediation}
	}
	_ = json.NewEncoder(out).Encode(result)
}
