package main

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
)

type methodDescription struct {
	Method                string         `json:"method"`
	Required              []string       `json:"required"`
	Optional              []string       `json:"optional,omitempty"`
	RequiresOperationID   bool           `json:"requiresOperationId"`
	RequiresLease         bool           `json:"requiresLease"`
	RequiresDocumentEpoch bool           `json:"requiresDocumentEpoch"`
	Capability            string         `json:"capability,omitempty"`
	Example               map[string]any `json:"example,omitempty"`
}

func runDescribe(opts options, stdout io.Writer) int {
	if len(opts.args) != 1 || opts.args[0] == "" {
		writeFailure(stdout, "INVALID_ARGUMENT", "describe requires exactly one RPC method.", "Use: browserctl --json describe METHOD")
		return 2
	}
	description, ok := describeMethod(opts.args[0])
	if !ok {
		writeFailure(stdout, "METHOD_NOT_FOUND", "No public RPC method named "+opts.args[0]+" is registered.", "Read the bundled API reference for supported methods.")
		return 1
	}
	_ = json.NewEncoder(stdout).Encode(map[string]any{"ok": true, "description": description})
	return 0
}

func describeMethod(method string) (methodDescription, bool) {
	if !publicRPCMethods[method] {
		return methodDescription{}, false
	}
	description := methodDescription{
		Method:                method,
		RequiresOperationID:   describedRequiresOperationID(method),
		RequiresLease:         describedRequiresLease(method),
		RequiresDocumentEpoch: describedRequiresDocumentEpoch(method),
		Capability:            describedCapability(method),
	}
	if description.RequiresLease {
		description.Required = append(description.Required, "sessionId", "tabId", "leaseId")
	}
	if description.RequiresOperationID {
		description.Required = append(description.Required, "operationId")
	}
	if description.RequiresDocumentEpoch {
		description.Required = append(description.Required, "expectedDocumentEpoch")
	}
	description.Required = append(description.Required, methodRequired[method]...)
	description.Optional = append(description.Optional, methodOptional[method]...)
	description.Required = uniqueSorted(description.Required)
	description.Optional = uniqueSorted(description.Optional)
	description.Example = methodExamples[method]
	return description, true
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func describedRequiresLease(method string) bool {
	if !strings.Contains(method, ".") || strings.HasPrefix(method, "session.") || strings.HasPrefix(method, "artifact.") || strings.HasPrefix(method, "operation.") || strings.HasPrefix(method, "event.") || strings.HasPrefix(method, "confirmation.") {
		return false
	}
	switch method {
	case "browser.list", "browser.history.query", "browser.stop", "tab.list", "tab.get", "tab.open", "tab.claim":
		return false
	default:
		return !strings.HasPrefix(method, "daemon.")
	}
}

func describedRequiresOperationID(method string) bool {
	switch method {
	case "tab.open", "tab.activate", "tab.close", "tab.navigate", "tab.back", "tab.forward", "tab.reload",
		"action.perform", "dialog.respond", "clipboard.write", "fileChooser.setFiles", "content.export", "pageAssets.export",
		"secureInput.request", "unsafe.evaluate", "unsafe.cdp.send":
		return true
	default:
		return false
	}
}

func describedRequiresDocumentEpoch(method string) bool {
	switch method {
	case "tab.navigate", "tab.back", "tab.forward", "tab.reload", "action.perform", "dialog.respond",
		"fileChooser.setFiles", "secureInput.request", "unsafe.evaluate", "unsafe.cdp.send":
		return true
	default:
		return false
	}
}

func describedCapability(method string) string {
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

var publicRPCMethods = map[string]bool{
	"daemon.hello": true, "daemon.status": true, "daemon.diagnostics": true,
	"session.open": true, "session.get": true, "session.close": true, "session.stop": true, "session.requestCapabilities": true,
	"browser.list": true, "browser.history.query": true, "browser.stop": true,
	"tab.list": true, "tab.get": true, "tab.open": true, "tab.claim": true, "tab.lease.renew": true, "tab.activate": true, "tab.release": true, "tab.close": true, "tab.navigate": true, "tab.back": true, "tab.forward": true, "tab.reload": true,
	"observation.capture": true, "observation.diff": true, "locator.query": true, "action.perform": true, "condition.wait": true,
	"dialog.get": true, "dialog.respond": true, "clipboard.read": true, "clipboard.write": true, "fileChooser.setFiles": true,
	"download.get": true, "download.wait": true, "content.export": true, "pageAssets.list": true, "pageAssets.export": true,
	"artifact.get": true, "artifact.readChunk": true, "artifact.delete": true,
	"operation.get": true, "operation.wait": true, "operation.cancel": true, "event.next": true, "event.replay": true,
	"confirmation.get": true, "confirmation.list": true, "secureInput.request": true, "unsafe.evaluate": true, "unsafe.cdp.send": true,
}

var methodRequired = map[string][]string{
	"session.get": {"sessionId"}, "session.close": {"sessionId"}, "session.stop": {"sessionId"}, "session.requestCapabilities": {"sessionId", "capabilities"},
	"browser.history.query": {"sessionId"},
	"tab.get":               {"tabId"}, "tab.open": {"sessionId"}, "tab.claim": {"sessionId", "tabId"}, "tab.lease.renew": {"sessionId", "tabId", "leaseId"},
	"tab.navigate": {"url"}, "observation.diff": {"snapshotId"}, "locator.query": {"locator"}, "action.perform": {"action"}, "condition.wait": {"condition"},
	"fileChooser.setFiles": {"files"}, "download.get": {"downloadId"}, "artifact.get": {"sessionId", "artifactId"},
	"artifact.readChunk": {"sessionId", "artifactId"}, "artifact.delete": {"sessionId", "artifactId"},
	"operation.get": {"sessionId", "operationId"}, "operation.wait": {"sessionId", "operationId"}, "operation.cancel": {"sessionId", "operationId"},
	"event.next": {"sessionId"}, "event.replay": {"sessionId"}, "confirmation.get": {"sessionId", "confirmationId"}, "confirmation.list": {"sessionId"},
	"secureInput.request": {"origin", "target"}, "unsafe.evaluate": {"expression"}, "unsafe.cdp.send": {"method"},
}

var methodOptional = map[string][]string{
	"session.open": {"name", "clientId"}, "session.requestCapabilities": {"browserInstanceId", "timeoutMs"},
	"browser.list": {"browserInstanceId"}, "browser.history.query": {"text", "startTime", "endTime", "maxResults"},
	"tab.list": {"sessionId", "browserInstanceId", "active", "currentWindow", "windowId"}, "tab.get": {"sessionId"}, "tab.open": {"url", "active", "windowId"},
	"tab.claim": {"browserInstanceId", "leaseTtlMs"}, "tab.lease.renew": {"leaseTtlMs"}, "tab.release": {"reason"},
	"observation.capture": {"maxNodes", "include", "screenshot"}, "locator.query": {"snapshotId"},
	"action.perform": {"confirmation", "confirmationId", "confirmationTimeoutMs", "expect", "observeAfter", "timeoutMs"}, "condition.wait": {"timeoutMs"},
	"dialog.respond": {"accept", "promptText"}, "clipboard.write": {"text", "html", "items"},
	"download.wait": {"downloadId", "timeoutMs"}, "content.export": {"format"},
	"artifact.get": {"includeLocalPath"}, "artifact.readChunk": {"offset", "length"}, "operation.wait": {"timeoutMs"},
	"event.next": {"timeoutMs"}, "event.replay": {"afterSeq"}, "secureInput.request": {"label", "secret", "autocomplete"},
	"unsafe.evaluate": {"awaitPromise", "returnByValue", "userGesture", "targetSessionId"}, "unsafe.cdp.send": {"params", "targetSessionId"},
}

var methodExamples = map[string]map[string]any{
	"observation.capture": {"sessionId": "ses_...", "tabId": "tab_...", "leaseId": "lease_...", "screenshot": false},
	"action.perform":      {"sessionId": "ses_...", "tabId": "tab_...", "leaseId": "lease_...", "operationId": "op_...", "expectedDocumentEpoch": 1, "action": map[string]any{"type": "press", "key": "PageDown"}},
	"content.export":      {"sessionId": "ses_...", "tabId": "tab_...", "leaseId": "lease_...", "operationId": "op_...", "format": "markdown"},
	"tab.activate":        {"sessionId": "ses_...", "tabId": "tab_...", "leaseId": "lease_...", "operationId": "op_..."},
}
