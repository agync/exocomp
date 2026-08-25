package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// This file mounts the UnifiedPush per-device push-endpoint routes (v1.1 push
// path). A device registers the URL of its UnifiedPush distributor and the
// daemon POSTs each notification to it. The URL is CLIENT-SUPPLIED, so the
// register handler runs it through notify.ValidatePushEndpoint — the FIRST of
// two SSRF layers (https-only, no credentials, no host that resolves to a
// private/loopback/link-local/metadata address). The SECOND layer lives in the
// transport's dialer and re-checks the actual connected IP at send time, which
// is the check DNS rebinding cannot defeat.
//
// Scope: register and unregister are operator writes, the same bearerOnly
// operator floor every other /api/v1 write uses (a browser session may not drive
// them). Ownership is by token: an endpoint is associated with the token that
// registered it, and a token lists and deletes ONLY its own — an id owned by a
// different token reads as not-found, so this face never lets one device
// enumerate or drop another's registration.

// maxPushEndpointsPerToken caps how many endpoints one token may register, so a
// compromised or buggy device cannot fan the daemon's outbound push volume out
// without bound. A device has a handful of distributors at most.
const maxPushEndpointsPerToken = 10

// maxPushLabelLen caps the optional label a device attaches to an endpoint.
const maxPushLabelLen = 200

// registerPushAPI mounts the notify-endpoint routes. The GET is a read (any
// authed token, scoped to its own rows); register/unregister are operator writes.
func (s *Server) registerPushAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/notify/endpoints", s.apiAuth(s.apiPushEndpoints))
	mux.HandleFunc("POST /api/v1/notify/endpoints",
		s.apiGuard(bearerOnly(canWrite), writeDeny("register push endpoints", "operator"), s.apiPushEndpointCreate))
	mux.HandleFunc("DELETE /api/v1/notify/endpoints/{id}",
		s.apiGuard(bearerOnly(canWrite), writeDeny("unregister push endpoints", "operator"), s.apiPushEndpointDelete))
}

// apiPushEndpoints lists the calling token's own registered endpoints. Scoping
// is by token id, so a device sees only what it registered.
func (s *Server) apiPushEndpoints(w http.ResponseWriter, r *http.Request) {
	ident, ok := identityOf(r.Context())
	if !ok {
		s.apiError(w, http.StatusInternalServerError, wire.NewError(wire.CodeInternal, "no resolved identity"))
		return
	}
	rows, err := s.store.ListPushEndpointsByToken(r.Context(), ident.token.ID)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindPushEndpointList, wire.NewPushEndpointList(rows))
}

// pushEndpointCreateRequest is the JSON body of a register. endpoint is the
// UnifiedPush distributor URL; label is an optional human tag.
type pushEndpointCreateRequest struct {
	Endpoint string `json:"endpoint"`
	Label    string `json:"label"`
}

// apiPushEndpointCreate registers an endpoint for the calling token after
// validating it against the SSRF deny-list. A rejected URL is a 400; a token at
// its cap is a 409.
func (s *Server) apiPushEndpointCreate(w http.ResponseWriter, r *http.Request) {
	ident, ok := identityOf(r.Context())
	if !ok {
		s.apiError(w, http.StatusInternalServerError, wire.NewError(wire.CodeInternal, "no resolved identity"))
		return
	}

	var req pushEndpointCreateRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.badRequest(w, "request body is not valid JSON")
			return
		}
	}
	if req.Endpoint == "" {
		s.badRequest(w, "endpoint is required")
		return
	}
	if len(req.Label) > maxPushLabelLen {
		s.badRequest(w, "label is too long")
		return
	}

	// SSRF layer one: refuse a non-https, credentialed, or private-resolving URL
	// before it is ever stored. The socket-level guard at send time is layer two.
	u, err := notify.ValidatePushEndpoint(r.Context(), req.Endpoint)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}

	count, err := s.store.CountPushEndpointsForToken(r.Context(), ident.token.ID)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if count >= maxPushEndpointsPerToken {
		s.apiError(w, http.StatusConflict, wire.NewError(wire.CodeConflict,
			"this token is at its push-endpoint cap; unregister one first"))
		return
	}

	e, err := s.store.CreatePushEndpoint(r.Context(), store.PushEndpoint{
		TokenID:   ident.token.ID,
		TokenName: ident.token.Name,
		URL:       u.String(),
		Label:     req.Label,
		CreatedAt: s.now(),
	})
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindPushEndpoint, wire.NewPushEndpointView(e))
}

// apiPushEndpointDelete unregisters an endpoint the calling token owns. An
// unknown id is a 404; an id owned by a different token is ALSO a 404, so a
// token can neither drop nor probe another device's registration.
func (s *Server) apiPushEndpointDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "endpoint id must be a positive integer")
		return
	}
	ident, ok := identityOf(r.Context())
	if !ok {
		s.apiError(w, http.StatusInternalServerError, wire.NewError(wire.CodeInternal, "no resolved identity"))
		return
	}
	e, err := s.store.GetPushEndpoint(r.Context(), id)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if e.TokenID != ident.token.ID {
		s.apiError(w, http.StatusNotFound, wire.NewError(wire.CodeNotFound, "no such endpoint"))
		return
	}
	if err := s.store.DeletePushEndpoint(r.Context(), id); err != nil {
		s.apiFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
