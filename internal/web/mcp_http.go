package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"exocomp.dev/exocomp/internal/mcp"
	"exocomp.dev/exocomp/internal/wire"
)

// This file mounts the MCP transport on the SAME ADR-0009 listener as /api/v1
// and the browser surface — one listener, one authentication story (ADR 0014 §3;
// agent-interface.md §3.5). It is a TRANSPORT only: the catalog, the handlers,
// and the scope floors all live in internal/mcp, and stdio runs the identical
// dispatch. The two faces differ in exactly two things — how a message is framed,
// and which Caller it is dispatched for.
//
// Shape: the streamable-HTTP single endpoint. One POST carries one client→server
// JSON-RPC message (or a batch of them) and the response comes back in that
// POST's body, as application/json.
//
// What is deliberately NOT implemented, and why:
//
//   - No GET stream. Streamable-HTTP allows a client to open a long-lived GET for
//     server→client messages; this server initiates none — it answers what it is
//     asked, and job progress is already an SSE tail at /api/v1/jobs/{id}/stream
//     with its own revoke-aware auth (finding AI-10). An idle stream would be a
//     socket held open for nothing, so GET is a 405.
//   - No sessions, and so no Mcp-Session-Id. A session id is state the server
//     would have to trust between requests, and it is exactly the thing that
//     would let a revoked or downgraded token keep working until it expired.
//     Statelessness is the security property here: every message re-runs the
//     bearer resolution (the same hardened path /api/v1 writes use, with its
//     backoff and KDF gate) and re-checks the scope, so a revoke bites on the
//     next call. An agent that wants continuity re-sends initialize; it is free.
//   - No server→client requests (sampling, roots, elicitation), for the same
//     reason as the GET stream: nothing here asks the client anything.

// mcpPath is the single streamable-HTTP endpoint.
const mcpPath = "/mcp"

// mcpMaxBody bounds one POSTed message. It matches the ceiling the stdio framer
// enforces per line, so neither transport accepts a message the other would not.
const mcpMaxBody = 8 << 20

// registerMCP mounts the MCP endpoint. Registering POST alone means Go's
// method-aware routing answers GET/DELETE with 405 — the documented "no
// server-initiated stream, no session to delete" behaviour, for free.
//
// The guard is the /api/v1 write guard: an explicit bearer (never the browser's
// ambient session, finding AI-8) of a scope this surface recognizes. Anything
// finer is per-tool and belongs to internal/mcp, which re-checks it per call.
func (s *Server) registerMCP(mux *http.ServeMux) {
	mux.HandleFunc("POST "+mcpPath, s.apiGuard(bearerOnly(canRead),
		"this request may not drive MCP: it needs an Authorization: Bearer token "+
			"(a browser session may not drive the machine faces)", s.handleMCP))
}

// mcpCaller projects the resolved identity onto the caller internal/mcp
// authorizes against. Only the token's NAME and row id travel — never its secret
// (invariant 4). The decision seam is attached under the approve scope alone;
// TokenCaller drops it for any other scope, so the W and A floors cannot be
// satisfied by one credential (finding AI-2).
func (s *Server) mcpCaller(r *http.Request) mcp.Caller {
	ident, ok := identityOf(r.Context())
	if !ok {
		// Unreachable behind apiGuard; a zero Caller authorizes nothing, so the
		// fallback fails closed rather than assuming a scope.
		return mcp.Caller{}
	}
	return mcp.TokenCaller(ident.token.Scope, ident.token.Name, ident.token.ID, nil)
}

// handleMCP dispatches the POSTed message (or batch) and writes the responses.
// A message that draws no response — a JSON-RPC notification — is acknowledged
// with 202 and an empty body, as the transport requires; there is no id to
// answer on, and inventing a frame would tell an agent something happened that
// the protocol says did not.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBody+1))
	if err != nil {
		s.badRequest(w, "cannot read the request body")
		return
	}
	if len(body) > mcpMaxBody {
		s.apiError(w, http.StatusRequestEntityTooLarge,
			wire.NewError(codeInvalidArgument, "message exceeds the maximum size"))
		return
	}

	caller := s.mcpCaller(r)
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		s.writeMCP(w, mcpParseError(), false)
		return
	}

	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil || len(batch) == 0 {
			s.writeMCP(w, mcpParseError(), false)
			return
		}
		var out []json.RawMessage
		for _, msg := range batch {
			if resp, write := s.mcpSrv.HandleMessage(r.Context(), caller, msg); write {
				out = append(out, resp)
			}
		}
		s.writeMCPBatch(w, out)
		return
	}

	resp, write := s.mcpSrv.HandleMessage(r.Context(), caller, trimmed)
	s.writeMCP(w, resp, !write)
}

// writeMCP writes one response frame, or acknowledges a notification.
func (s *Server) writeMCP(w http.ResponseWriter, resp json.RawMessage, acked bool) {
	if acked {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(resp); err != nil {
		s.log.Error("mcp: write response", "err", err)
	}
}

// writeMCPBatch writes the responses to a batch. A batch of nothing but
// notifications draws no frames at all, and so is acknowledged like one.
func (s *Server) writeMCPBatch(w http.ResponseWriter, out []json.RawMessage) {
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	body, err := json.Marshal(out)
	if err != nil {
		s.log.Error("mcp: marshal batch", "err", err)
		s.writeMCP(w, mcpParseError(), false)
		return
	}
	s.writeMCP(w, body, false)
}

// mcpParseError is the JSON-RPC frame for a body that could not be parsed at
// all. It is a protocol error on a 200, not an HTTP error: the request itself
// was authorized and delivered, its content was not JSON-RPC.
func mcpParseError() json.RawMessage {
	return json.RawMessage(`{"jsonrpc":"2.0","id":null,"error":` +
		`{"code":-32700,"message":"parse error"}}`)
}
