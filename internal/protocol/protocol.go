package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const Version = "1.0"

// Request and Response intentionally use RawMessage. The JSON schema in
// packages/protocol is the wire-format source of truth; the daemon remains a
// transport and policy boundary instead of duplicating every browser shape.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type ErrorData struct {
	Kind      string         `json:"kind"`
	Retryable bool           `json:"retryable"`
	Effect    string         `json:"effect"`
	Details   map[string]any `json:"details,omitempty"`
}

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodeSessionNotFound    = -32001
	CodeSessionStopped     = -32002
	CodeTabNotFound        = -32003
	CodeLeaseConflict      = -32004
	CodeLeaseRequired      = -32005
	CodeLeaseExpired       = -32006
	CodeOperationConflict  = -32007
	CodeBridgeUnavailable  = -32008
	CodeBridgeTimeout      = -32009
	CodeStaleReference     = -32010
	CodeCapabilityDenied   = -32011
	CodeInvalidState       = -32012
	CodeArtifactNotFound   = -32013
	CodeConfirmationNeeded = -32014
	CodeUnsupported        = -32015
	CodeCancelled          = -32016
	CodeProtocolMismatch   = -32017
	CodeEffectUncertain    = -32018
	CodeFileNotAllowed     = -32019
)

func NewError(code int, name, message string, retryable bool, details map[string]any) *RPCError {
	dataMap := map[string]any{"kind": name, "retryable": retryable, "effect": "none"}
	for key, value := range details {
		dataMap[key] = value
	}
	data, _ := json.Marshal(dataMap)
	return &RPCError{Code: code, Message: message, Data: data}
}

// WithEffect returns a copy of rpcErr with effect and additional structured
// fields set. It is used after a request has crossed the extension boundary,
// where a timeout can no longer prove that a side effect did not happen.
func WithEffect(rpcErr *RPCError, effect string, details map[string]any) *RPCError {
	if rpcErr == nil {
		return nil
	}
	copy := *rpcErr
	data := make(map[string]any)
	if len(rpcErr.Data) > 0 {
		_ = json.Unmarshal(rpcErr.Data, &data)
	}
	if data["kind"] == nil {
		data["kind"] = "INTERNAL"
	}
	if data["retryable"] == nil {
		data["retryable"] = false
	}
	data["effect"] = effect
	for key, value := range details {
		data[key] = value
	}
	copy.Data, _ = json.Marshal(data)
	return &copy
}

func ErrorKind(rpcErr *RPCError) string {
	if rpcErr == nil {
		return ""
	}
	var data map[string]any
	if json.Unmarshal(rpcErr.Data, &data) == nil {
		if kind, ok := data["kind"].(string); ok {
			return kind
		}
	}
	return ""
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func InvalidParams(message string, details map[string]any) *RPCError {
	return NewError(CodeInvalidParams, "INVALID_REQUEST", message, false, details)
}

func Internal(err error) *RPCError {
	// Do not reflect arbitrary internal errors across the RPC boundary: they can
	// contain request values, filesystem paths, or browser content. Callers get a
	// stable kind while the owning process can log the wrapped error if desired.
	_ = err
	return NewError(CodeInternalError, "INTERNAL", "internal error", false, nil)
}

func MarshalResult(value any) json.RawMessage {
	if value == nil {
		return json.RawMessage("null")
	}
	if raw, ok := value.(json.RawMessage); ok {
		return raw
	}
	b, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func IDKey(id json.RawMessage) string {
	return string(bytes.TrimSpace(id))
}

func ValidateRequest(req Request) error {
	if req.JSONRPC != "2.0" {
		return fmt.Errorf("jsonrpc must be 2.0")
	}
	if req.Method == "" {
		return fmt.Errorf("method is required")
	}
	if len(req.ID) > 0 && !json.Valid(req.ID) {
		return fmt.Errorf("invalid id")
	}
	return nil
}

func IsNotification(req Request) bool {
	return len(bytes.TrimSpace(req.ID)) == 0
}
