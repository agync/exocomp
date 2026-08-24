package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
)

// This file hand-rolls the JSON-RPC 2.0 message shape rather than taking an SDK
// dependency: the surface is initialize + five methods, and keeping go.mod
// clean is a stated goal — an SDK would be more code to audit than the
// dispatch below. HandleMessage is the one per-message seam; the HTTP
// transport (internal/web/mcp_http.go) frames a POST body around it.

// JSON-RPC 2.0 error codes. The four standard codes plus the transport does not
// invent any application codes: a tool/resource failure is carried inside a
// successful result as a wire error envelope, not as a protocol error, so the
// model sees the taxonomy code it branches on.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// rpcRequest is one decoded JSON-RPC message. A request carries an id and
// expects a response; a notification omits id and gets none. Params and id stay
// raw so id echoes back verbatim (number or string) and params decode per method.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the message wants no response. A JSON-RPC
// notification omits id entirely.
func (r rpcRequest) isNotification() bool { return len(r.ID) == 0 }

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// nullID is the id of a response to a message we could not parse an id from.
var nullID = json.RawMessage("null")

// HandleMessage dispatches ONE framed JSON-RPC message for a caller and returns
// the response bytes. It is the one per-message seam the HTTP transport mounts
// on (internal/web/mcp_http.go), which frames a POST body around it but runs
// this exact dispatch. write is false for a notification, which by JSON-RPC
// gets no response at all.
func (s *Server) HandleMessage(ctx context.Context, c Caller, msg []byte) (json.RawMessage, bool) {
	resp, write := s.handleLine(ctx, c, msg)
	if !write {
		return nil, false
	}
	var buf bytes.Buffer
	if err := writeMessage(&buf, resp); err != nil {
		fallback, _ := json.Marshal(errorResponse(nullID, codeInternalError, "marshal response"))
		return fallback, true
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}

// handleLine decodes one framed message and dispatches it. It returns the
// response to write and whether to write at all (a notification, or a message
// whose id we cannot recover, produces no response — except a parse error,
// which JSON-RPC answers with a null-id error).
func (s *Server) handleLine(ctx context.Context, c Caller, line []byte) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nullID, codeParseError, "parse error"), true
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if req.isNotification() {
			return rpcResponse{}, false
		}
		return errorResponse(req.ID, codeInvalidRequest, "invalid request"), true
	}

	if req.isNotification() {
		if sideEffecting(req.Method) {
			// A method that can mutate state must be answerable: sent as a
			// notification it used to enqueue a job and write ZERO bytes back,
			// leaving nothing for the caller — or an audit wrapper sitting on
			// the frames — to record. Refuse instead, loudly.
			return errorResponse(nullID, codeInvalidRequest,
				"method "+req.Method+" must carry an id: a side-effecting call is not a notification"), true
		}
		// Any other notification gets no reply: there is no id to answer.
		_, _ = s.dispatch(ctx, c, req.Method, req.Params)
		return rpcResponse{}, false
	}

	result, rerr := s.dispatch(ctx, c, req.Method, req.Params)
	if rerr != nil {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr}, true
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "marshal result"), true
	}
	return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: raw}, true
}

// sideEffecting reports whether a method may change state or run work, and so
// must carry an id the caller can be answered on.
func sideEffecting(method string) bool {
	switch method {
	case "tools/call", "resources/read":
		return true
	default:
		return false
	}
}

func errorResponse(id json.RawMessage, code int, msg string) rpcResponse {
	if len(id) == 0 {
		id = nullID
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// writeMessage encodes one response as a single line. SetEscapeHTML(false)
// keeps identifiers and states unescaped, matching the wire encoder, and the
// encoder appends the terminating newline that frames the message.
func writeMessage(w io.Writer, resp rpcResponse) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(resp)
}
