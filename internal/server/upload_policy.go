package server

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cc-hchi/browser-control/internal/protocol"
)

const maxUploadFiles = 100

// validateUploadFiles enforces the daemon-side upload allowlist and returns
// canonical paths safe to forward to Chrome. The legacy "paths" alias is
// intentionally validated too because the extension accepts it when "files"
// is absent; otherwise it would bypass this policy boundary.
func validateUploadFiles(params map[string]any, configuredRoots []string) ([]string, *protocol.RPCError) {
	rawFiles, ok := params["files"]
	if !ok {
		rawFiles, ok = params["paths"]
	}
	files, ok := rawFiles.([]any)
	if !ok || len(files) == 0 || len(files) > maxUploadFiles {
		return nil, fileNotAllowed("invalid_files", -1)
	}

	roots := resolveUploadRoots(configuredRoots)
	if len(roots) == 0 {
		return nil, fileNotAllowed("no_available_upload_root", -1)
	}

	resolvedFiles := make([]string, 0, len(files))
	for index, value := range files {
		path, ok := value.(string)
		if !ok || path == "" || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) {
			return nil, fileNotAllowed("invalid_path", index)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
		if err != nil {
			return nil, fileNotAllowed("not_found", index)
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return nil, fileNotAllowed("invalid_path", index)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fileNotAllowed("not_found", index)
		}
		if !info.Mode().IsRegular() {
			return nil, fileNotAllowed("not_regular", index)
		}
		if !withinAnyUploadRoot(resolved, roots) {
			return nil, fileNotAllowed("outside_upload_roots", index)
		}
		resolvedFiles = append(resolvedFiles, resolved)
	}
	return resolvedFiles, nil
}

func resolveUploadRoots(configuredRoots []string) []string {
	resolved := make([]string, 0, len(configuredRoots))
	seen := make(map[string]struct{}, len(configuredRoots))
	for _, root := range configuredRoots {
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		canonical, err = filepath.Abs(canonical)
		if err != nil {
			continue
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			continue
		}
		canonical = filepath.Clean(canonical)
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		resolved = append(resolved, canonical)
	}
	return resolved
}

func withinAnyUploadRoot(path string, roots []string) bool {
	for _, root := range roots {
		relative, err := filepath.Rel(root, path)
		if err != nil || filepath.IsAbs(relative) {
			continue
		}
		if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

func fileNotAllowed(reason string, index int) *protocol.RPCError {
	details := map[string]any{"reason": reason}
	if index >= 0 {
		details["index"] = index
	}
	return protocol.NewError(protocol.CodeFileNotAllowed, "FILE_NOT_ALLOWED", "upload file is not allowed", false, details)
}
