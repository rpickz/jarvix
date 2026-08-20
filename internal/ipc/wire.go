// Package ipc implements Jarvix's daemon↔client protocol: JSON-RPC 2.0 over
// a Unix domain socket at $XDG_RUNTIME_DIR/jarvix.sock, newline-delimited.
//
// Clients send requests (session.start, status.get, ...) and receive
// responses; the daemon additionally pushes events (state.changed,
// assistant.delta, ...) to every connected client as JSON-RPC notifications.
// The protocol is documented in docs/ipc.md.
package ipc

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion identifies the wire protocol. Bumped on incompatible
// changes; reported by status.get so clients can detect mismatches.
const ProtocolVersion = 1

// Request is a JSON-RPC 2.0 request. A nil ID marks a notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

// Notification is a server-initiated event push.
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error implements the error interface.
func (e *Error) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Standard JSON-RPC error codes, plus Jarvix's application range.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// CodeSessionError covers session-level failures (no active session,
	// invalid state for the operation).
	CodeSessionError = -32000
)

// Errorf builds an *Error.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
