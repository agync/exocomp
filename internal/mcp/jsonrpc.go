package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// The stdio transport frames each JSON-RPC 2.0 message as one line of JSON
// (newline-delimited), the MCP 2024-11-05 stdio shape. We hand-roll it rather
// than take an SDK dependency: the surface is initialize + five methods, the
// framing is one ReadBytes('\n'), and keeping go.mod clean is a stated goal —
// an SDK would be more code to audit than the loop below.

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

// maxLine bounds one framed message. Health and job-event lists are the largest
// payloads and sit far under this; the cap only stops a runaway line from
// exhausting memory.
const maxLine = 8 << 20

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

// serve runs the read-dispatch-write loop over a line-delimited transport until
// the reader is exhausted or ctx is cancelled. It is single-goroutine: every
// write happens on this goroutine, so no output interleaving is possible.
func (s *Server) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	br := bufio.NewReaderSize(in, 64<<10)
	bw := bufio.NewWriter(out)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := readLine(br)
		if len(line) == 0 && errors.Is(err, io.EOF) {
			return nil
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if errors.Is(err, io.EOF) {
				return nil
			}
			continue
		}

		resp, write := s.handleLine(ctx, line)
		if write {
			if werr := writeMessage(bw, resp); werr != nil {
				return werr
			}
			if ferr := bw.Flush(); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

// handleLine decodes one framed message and dispatches it. It returns the
// response to write and whether to write at all (a notification, or a message
// whose id we cannot recover, produces no response — except a parse error,
// which JSON-RPC answers with a null-id error).
func (s *Server) handleLine(ctx context.Context, line []byte) (rpcResponse, bool) {
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

	result, rerr := s.dispatch(ctx, req.Method, req.Params)
	if req.isNotification() {
		// A notification gets no reply, even on error: there is no id to answer.
		return rpcResponse{}, false
	}
	if rerr != nil {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr}, true
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return errorResponse(req.ID, codeInternalError, "marshal result"), true
	}
	return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: raw}, true
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

// readLine reads one newline-terminated frame, tolerating a line longer than
// the reader's buffer up to maxLine. It returns the bytes read and any
// terminal error (io.EOF once the stream ends).
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			// Drain to the next newline is not attempted: a message this large is
			// a broken peer, and returning stops the loop cleanly.
			return buf[:maxLine], errors.New("mcp: message exceeds max line size")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf, err
	}
}
