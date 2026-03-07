package agent

import (
	"encoding/json"
	"fmt"
)

// JSON-RPC 2.0 method constants (Codex app-server protocol).
const (
	methodInitialize    = "initialize"
	methodInitialized   = "initialized"
	methodThreadStart   = "thread/start"
	methodTurnStart     = "turn/start"
	methodTurnInterrupt = "turn/interrupt"

	methodTurnCompleted = "turn/completed"
	methodTurnFailed    = "turn/failed"
	methodTurnCancelled = "turn/cancelled"
	methodTokenUsage    = "thread/tokenUsage/updated"
	methodRateLimits    = "account/rateLimits/updated"

	methodCmdApproval  = "item/commandExecution/requestApproval"
	methodFileApproval = "item/fileChange/requestApproval"
	methodUserInput    = "item/tool/requestUserInput"
	methodToolCall     = "item/tool/call"
)

// rpcEnvelope is used for initial type detection.
type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Message types.

type Request struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID      any                    `json:"id"`
	Method  string                 `json:"method"`
	Params  map[string]any         `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Result  map[string]any `json:"result"`
}

type ErrorResponse struct {
	JSONRPC string   `json:"jsonrpc"`
	ID      any      `json:"id"`
	Error   rpcError `json:"error"`
}

type Notification struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

// Incoming is a discriminated union of what the server can send us.
type Incoming struct {
	// Exactly one of the following is non-nil.
	Response  *Response
	ErrResp   *ErrorResponse
	Notif     *Notification
	ServerReq *Request // server-initiated request (approval, user input)
}

// ParseLine parses one newline-delimited JSON-RPC message.
func ParseLine(line []byte) (*Incoming, error) {
	var env rpcEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}

	inc := &Incoming{}

	if env.Error != nil {
		// Error response.
		var id any
		_ = json.Unmarshal(env.ID, &id)
		inc.ErrResp = &ErrorResponse{
			JSONRPC: env.JSONRPC,
			ID:      id,
			Error:   *env.Error,
		}
		return inc, nil
	}

	if env.Result != nil {
		// Success response.
		var id any
		_ = json.Unmarshal(env.ID, &id)
		var result map[string]any
		_ = json.Unmarshal(env.Result, &result)
		inc.Response = &Response{JSONRPC: env.JSONRPC, ID: id, Result: result}
		return inc, nil
	}

	if env.Method != "" {
		var params map[string]any
		if len(env.Params) > 0 {
			_ = json.Unmarshal(env.Params, &params)
		}
		if len(env.ID) > 0 && string(env.ID) != "null" {
			// Server-initiated request.
			var id any
			_ = json.Unmarshal(env.ID, &id)
			inc.ServerReq = &Request{JSONRPC: env.JSONRPC, ID: id, Method: env.Method, Params: params}
		} else {
			// Notification.
			inc.Notif = &Notification{JSONRPC: env.JSONRPC, Method: env.Method, Params: params}
		}
		return inc, nil
	}

	return nil, fmt.Errorf("unrecognized JSON-RPC message")
}

// FormatRequest serializes a request to a newline-terminated JSON line.
func FormatRequest(id any, method string, params map[string]any) ([]byte, error) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// FormatNotification serializes a notification to a newline-terminated JSON line.
func FormatNotification(method string, params map[string]any) ([]byte, error) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// FormatResponse serializes a response to a newline-terminated JSON line.
func FormatResponse(id any, result map[string]any) ([]byte, error) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// FormatErrorResponse serializes an error response.
func FormatErrorResponse(id any, code int, message string) ([]byte, error) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
