package mcp

import (
	"bytes"
	"encoding/json"
)

// JSON-RPC 2.0 error codes used by MCP.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// message is any JSON-RPC 2.0 message: a request (method + id), a
// notification (method, no id) or a response (id + result/error).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m *message) isRequest() bool { return m.Method != "" && hasID(m.ID) }

func hasID(id json.RawMessage) bool {
	return len(id) > 0 && !bytes.Equal(id, []byte("null"))
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func resultResponse(id json.RawMessage, v any) response {
	return response{JSONRPC: "2.0", ID: id, Result: v}
}

func errorResponse(id json.RawMessage, code int, msg string) response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// parseBody splits a POST body into messages. MCP 2025-03-26 allows a
// JSON-RPC batch (array); later revisions send one message per request.
// Both are accepted.
func parseBody(body []byte) (msgs []*message, batch bool, err error) {
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var raw []json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, true, err
		}
		for _, r := range raw {
			var m message
			if err := json.Unmarshal(r, &m); err != nil {
				return nil, true, err
			}
			msgs = append(msgs, &m)
		}
		return msgs, true, nil
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, err
	}
	return []*message{&m}, false, nil
}
