package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cc-hchi/browser-control/internal/protocol"
)

func TestValidateUploadFilesAllowsRegularFilesAndCanonicalizesSymlinks(t *testing.T) {
	root := t.TempDir()
	realFile := filepath.Join(root, "nested", "upload.txt")
	if err := os.MkdirAll(filepath.Dir(realFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realFile, []byte("upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "upload-link.txt")
	if err := os.Symlink(realFile, symlink); err != nil {
		t.Fatal(err)
	}

	got, rpcErr := validateUploadFiles(map[string]any{"files": []any{symlink, realFile}}, []string{root})
	if rpcErr != nil {
		t.Fatalf("validateUploadFiles() error = %v", rpcErr)
	}
	canonicalFile, err := filepath.EvalSymlinks(realFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{canonicalFile, canonicalFile}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

func TestValidateUploadFilesRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "escape.txt")
	if err := os.Symlink(secret, escape); err != nil {
		t.Fatal(err)
	}

	_, rpcErr := validateUploadFiles(map[string]any{"files": []any{escape}}, []string{root})
	assertFileNotAllowed(t, rpcErr, "outside_upload_roots")
}

func TestValidateUploadFilesRejectsSiblingPrefix(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "allowed")
	sibling := filepath.Join(parent, "allowed-other")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sibling, "upload.txt")
	if err := os.WriteFile(file, []byte("upload"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, rpcErr := validateUploadFiles(map[string]any{"files": []any{file}}, []string{root})
	assertFileNotAllowed(t, rpcErr, "outside_upload_roots")
}

func TestValidateUploadFilesRejectsInvalidEntries(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		params map[string]any
		reason string
	}{
		{name: "missing array", params: map[string]any{}, reason: "invalid_files"},
		{name: "empty array", params: map[string]any{"files": []any{}}, reason: "invalid_files"},
		{name: "relative path", params: map[string]any{"files": []any{"relative.txt"}}, reason: "invalid_path"},
		{name: "non string", params: map[string]any{"files": []any{7}}, reason: "invalid_path"},
		{name: "missing file", params: map[string]any{"files": []any{filepath.Join(root, "missing.txt")}}, reason: "not_found"},
		{name: "directory", params: map[string]any{"files": []any{directory}}, reason: "not_regular"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, rpcErr := validateUploadFiles(test.params, []string{root})
			assertFileNotAllowed(t, rpcErr, test.reason)
		})
	}
}

func TestValidateUploadFilesValidatesLegacyPathsAlias(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "upload.txt")
	if err := os.WriteFile(file, []byte("upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, rpcErr := validateUploadFiles(map[string]any{"paths": []any{file}}, []string{root})
	if rpcErr != nil {
		t.Fatalf("validateUploadFiles() error = %v", rpcErr)
	}
	canonicalFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{canonicalFile}) {
		t.Fatalf("files = %v, want [%s]", got, canonicalFile)
	}
}

func TestForwardRejectsDisallowedUploadBeforeBridge(t *testing.T) {
	server, _, _, _ := startTestServer(t)
	session := server.core.OpenSession("upload-policy", "test")
	confirmation, rpcErr := server.core.CreateCapabilityConfirmation(session.SessionID, "test-browser", []string{"files.upload"}, time.Minute)
	if rpcErr != nil {
		t.Fatalf("CreateCapabilityConfirmation() error = %v", rpcErr)
	}
	if _, rpcErr = server.core.ResolveConfirmation(confirmation.ConfirmationID, "approve"); rpcErr != nil {
		t.Fatalf("ResolveConfirmation() error = %v", rpcErr)
	}
	lease, rpcErr := server.core.ClaimTab(session.SessionID, "tab-upload", time.Minute)
	if rpcErr != nil {
		t.Fatalf("ClaimTab() error = %v", rpcErr)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{
		"sessionId": session.SessionID,
		"tabId":     "tab-upload",
		"leaseId":   lease.LeaseID,
		"files":     []any{outside},
	}
	_, rpcErr = server.forward(context.Background(), "fileChooser.setFiles", protocol.MarshalResult(params), params, true)
	assertFileNotAllowed(t, rpcErr, "outside_upload_roots")
}

func assertFileNotAllowed(t *testing.T, rpcErr *protocol.RPCError, reason string) {
	t.Helper()
	if rpcErr == nil {
		t.Fatal("RPC error = nil, want FILE_NOT_ALLOWED")
	}
	if rpcErr.Code != protocol.CodeFileNotAllowed || protocol.ErrorKind(rpcErr) != "FILE_NOT_ALLOWED" {
		t.Fatalf("RPC error = %+v (kind %q), want code %d kind FILE_NOT_ALLOWED", rpcErr, protocol.ErrorKind(rpcErr), protocol.CodeFileNotAllowed)
	}
	var data map[string]any
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatalf("decode error data: %v", err)
	}
	if data["reason"] != reason || data["retryable"] != false || data["effect"] != "none" {
		t.Fatalf("error data = %v, want reason %q, retryable false, effect none", data, reason)
	}
}
