package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// Resources are the stable, cacheable "be aware of" surface an agent fetches or
// polls; tools operate. Each resource read routes to the SAME shared read helper
// its sibling tool calls, so a resource's bytes equal that tool's bytes and the
// CLI's --output json. The mimeType is application/json and the body is the wire
// envelope, unchanged.

const uriScheme = "exocomp://"

// resourceMIME is the content type of every resource body.
const resourceMIME = "application/json"

// staticResource is a concrete, non-templated resource for resources/list.
type staticResource struct {
	uri  string
	name string
	desc string
}

func (s *Server) staticResources() []staticResource {
	return []staticResource{
		{"exocomp://workspaces", "workspaces", "All tofu workspaces."},
		{"exocomp://approvals/pending", "pending approvals", "Approvals awaiting a decision."},
		{"exocomp://schedules", "schedules", "All schedules."},
		{"exocomp://events/recent", "recent events", "The most recent received events."},
		{"exocomp://health", "health", "Doctor subsystem summary, including the reactive kill-switch."},
	}
}

// resourceTemplate is a URI-templated resource for resources/templates/list. The
// per-entity resources (a specific job, a workspace's drift) are templates: an
// agent fills the id or name.
type resourceTemplate struct {
	uriTemplate string
	name        string
	desc        string
}

func (s *Server) resourceTemplates() []resourceTemplate {
	return []resourceTemplate{
		{"exocomp://jobs/{id}", "job", "One job by id."},
		{"exocomp://jobs/{id}/log", "job log", "One job's append-only, already-redacted event log."},
		{"exocomp://workspaces/{name}/drift", "workspace drift", "A workspace's newest-plan drift (values suppressed)."},
	}
}

func (s *Server) handleResourcesList() map[string]any {
	list := s.staticResources()
	out := make([]map[string]any, 0, len(list))
	for _, r := range list {
		out = append(out, map[string]any{
			"uri":         r.uri,
			"name":        r.name,
			"description": r.desc,
			"mimeType":    resourceMIME,
		})
	}
	return map[string]any{"resources": out}
}

func (s *Server) handleResourceTemplatesList() map[string]any {
	list := s.resourceTemplates()
	out := make([]map[string]any, 0, len(list))
	for _, r := range list {
		out = append(out, map[string]any{
			"uriTemplate": r.uriTemplate,
			"name":        r.name,
			"description": r.desc,
			"mimeType":    resourceMIME,
		})
	}
	return map[string]any{"resourceTemplates": out}
}

// readParams is the resources/read request body.
type readParams struct {
	URI string `json:"uri"`
}

// handleResourcesRead resolves a resource URI to its shared read helper and
// returns the wire envelope as the resource body. An unknown URI or a malformed
// id is an invalid-params protocol error; a store failure (a missing job) is
// carried as a protocol error whose message is the taxonomy code, since a
// resource body has no isError flag to distinguish an error envelope from data.
func (s *Server) handleResourcesRead(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p readParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid resources/read params"}
	}
	kind, data, ok, err := s.resolveResource(ctx, p.URI)
	if !ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown resource: " + p.URI}
	}
	if err != nil {
		var e wire.Error
		if ae, isArg := err.(argError); isArg {
			e = wire.NewError(codeInvalidArgument, ae.Error())
		} else {
			e = wire.ErrorFor(err)
		}
		return nil, &rpcError{Code: codeInternalError, Message: e.Code + ": " + e.Message}
	}
	b, merr := wire.MarshalSuccess(kind, data)
	if merr != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "marshal resource"}
	}
	return map[string]any{
		"contents": []map[string]any{{
			"uri":      p.URI,
			"mimeType": resourceMIME,
			"text":     string(b),
		}},
	}, nil
}

// resolveResource maps a URI to its read. ok is false for a URI that names no
// resource at all (so the caller returns unknown-resource); a recognized URI
// with a bad id returns ok=true and an argError.
func (s *Server) resolveResource(ctx context.Context, uri string) (kind string, data any, ok bool, err error) {
	if !strings.HasPrefix(uri, uriScheme) {
		return "", nil, false, nil
	}
	path := strings.TrimPrefix(uri, uriScheme)
	segs := strings.Split(path, "/")

	switch {
	case path == "workspaces":
		k, d, e := s.readWorkspaces(ctx)
		return k, d, true, e
	case path == "approvals/pending":
		k, d, e := s.readApprovals(ctx, true)
		return k, d, true, e
	case path == "schedules":
		k, d, e := s.readSchedules(ctx)
		return k, d, true, e
	case path == "events/recent":
		k, d, e := s.readEvents(ctx, recentEventsFilter())
		return k, d, true, e
	case path == "health":
		k, d, e := s.readHealth(ctx)
		return k, d, true, e
	case len(segs) == 2 && segs[0] == "jobs":
		id, perr := strconv.ParseInt(segs[1], 10, 64)
		if perr != nil || id <= 0 {
			return "", nil, true, invalidArg("job id must be a positive integer")
		}
		k, d, e := s.readJob(ctx, id)
		return k, d, true, e
	case len(segs) == 3 && segs[0] == "jobs" && segs[2] == "log":
		id, perr := strconv.ParseInt(segs[1], 10, 64)
		if perr != nil || id <= 0 {
			return "", nil, true, invalidArg("job id must be a positive integer")
		}
		k, d, e := s.readJobEvents(ctx, id, 0)
		return k, d, true, e
	case len(segs) == 3 && segs[0] == "workspaces" && segs[2] == "drift":
		if segs[1] == "" {
			return "", nil, true, invalidArg("workspace name is required")
		}
		k, d, e := s.readDrift(ctx, segs[1])
		return k, d, true, e
	default:
		return "", nil, false, nil
	}
}

// recentEventsFilter is the events/recent resource's read: the newest events,
// matching the list_events default of 25.
func recentEventsFilter() store.EventFilter { return store.EventFilter{Limit: 25} }
