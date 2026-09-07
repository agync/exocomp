package web

import (
	"errors"
	"net/http"
	"strings"

	"exocomp.dev/exocomp/internal/store"
)

// A netgroup names a set of CIDRs — the operator's own vocabulary for
// classifying an address, never a secret. So, like schedules, this lane is
// operator-scope only: no reauth, no confirm_token. The prefixes field is
// parsed by the shared store.ParsePrefixList, so a group added from the
// browser is held to exactly the same rules as one added from the CLI.

func init() { registerWrite((*Server).registerNetgroupWrite) }

func (s *Server) registerNetgroupWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /netgroups/add", s.requireAuth(s.handleNetgroupAdd))
	mux.HandleFunc("POST /netgroups/{name}/rm", s.requireAuth(s.handleNetgroupRm))
}

func (s *Server) handleNetgroupAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PostFormValue("name")
	cidrs := r.PostFormValue("cidrs")
	form := netgroupFormVM{Name: name, CIDRs: cidrs}

	// Validate both fields up front so every refusal renders at once (the
	// daemon stays authoritative: these client rules only shape the response).
	prefixes, parseErr := store.ParsePrefixList(cidrs)
	if !store.ValidName(name) {
		form.ErrName, form.FixName = netgroupNameRefusal(name)
	}
	if parseErr != nil {
		s.log.Info("web: netgroup add refused", "err", parseErr.Error(), "by", ident.token.Name)
		form.ErrCIDRItem, form.ErrCIDRs = netgroupCIDRRefusal(cidrs, parseErr)
	}
	if form.ErrName != "" || form.ErrCIDRs != "" {
		form.FirstInvalid = "name"
		if form.ErrName == "" {
			form.FirstInvalid = "cidrs"
		}
		s.netgroupRefusal(w, r, form)
		return
	}

	created, err := s.store.CreateNetgroup(r.Context(), store.NewNetgroup{
		Name: name, Prefixes: prefixes,
	})
	if errors.Is(err, store.ErrDuplicate) {
		s.log.Info("web: netgroup add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrName = "A netgroup called " + name + " already exists."
		form.FirstInvalid = "name"
		s.netgroupRefusal(w, r, form)
		return
	}
	if err != nil {
		s.log.Error("web: netgroup add", "err", err.Error())
		form.ErrForm = netgroupSubmitRefusal
		s.netgroupRefusal(w, r, form)
		return
	}
	s.log.Info("web: netgroup add", "netgroup", created.Name, "by", ident.token.Name)
	redirectFlash(w, r, "/checks?tab=netgroups", netgroupAddedFlash(created.Name, len(created.Prefixes)))
}

// netgroupRefusal re-renders the checks surface at 422 with the add form
// carrying its refusals — the JS-off path, and the fragment body a JS submit
// swaps in place. Refusals never flash: flash is success-only (write.go).
func (s *Server) netgroupRefusal(w http.ResponseWriter, r *http.Request, form netgroupFormVM) {
	vm, err := s.checksVMFor(r, &form)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, checksList(vm))
}

func (s *Server) handleNetgroupRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if !r.Form.Has("confirmed") {
		http.Error(w, "delete requires confirmation", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeleteNetgroup(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no netgroup named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: netgroup rm", "netgroup", name, "by", ident.token.Name)
	redirectFlash(w, r, "/checks?tab=netgroups", "removed "+name)
}

// netgroupSubmitRefusal is the whole-submit status line (forms/copy.txt): the
// daemon did not take the netgroup and nothing was saved. The raw cause goes
// to the log at the same call site — never to the operator.
const netgroupSubmitRefusal = "Couldn't reach exocompd — nothing was saved."

// netgroupAddedFlash is the one legitimate success flash (forms/copy.txt):
// what was added and how many ranges, shown only after the dialog closed. The
// flash cookie is ASCII-only (Cookie.String sanitizes non-ASCII bytes to
// spaces), so the copy's "·" separator reads as a parenthetical here.
func netgroupAddedFlash(name string, n int) string {
	word := "ranges"
	if n == 1 {
		word = "range"
	}
	return name + " added (" + itoa(int64(n)) + " " + word + ")"
}

// netgroupNameRefusal translates a refused netgroup name into the field's
// human sentence (forms/copy.txt), plus a one-click safe name when the input
// suggests one. The validator's own words never reach the operator.
func netgroupNameRefusal(name string) (msg, fix string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Give the netgroup a name.", ""
	}
	if strings.Contains(name, " ") {
		msg = "Names can't have spaces."
	} else {
		msg = "Names use letters, digits, dots, hyphens and underscores."
	}
	return msg, netgroupSlug(name)
}

// netgroupSlug derives the fix link's value per the copy's slug rule:
// lowercase, trim, collapse every run of characters outside [A-Za-z0-9_.-] to
// a dash, strip edge dashes. It is offered only when the result is non-empty
// and differs from the input.
func netgroupSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" || slug == s {
		return ""
	}
	return slug
}

// netgroupCIDRRefusal translates a ParsePrefixList refusal into the CIDR
// field's sentence (forms/copy.txt). The offending member returns separately
// (rendered mono ahead of the rest); a list with no usable member — an empty
// field or a double comma — returns the required sentence with no item. The
// parser's raw words stay in the error, which the caller logs.
func netgroupCIDRRefusal(field string, err error) (item, msg string) {
	raw := err.Error()
	if strings.TrimSpace(field) == "" || strings.Contains(raw, "empty CIDR") {
		return "", "Add at least one range, like 192.168.0.0/16."
	}
	item = raw
	if rest, ok := strings.CutPrefix(raw, `"`); ok {
		if end := strings.IndexByte(rest, '"'); end >= 0 {
			item = rest[:end]
		}
	}
	switch {
	case strings.Contains(raw, "no '/'"):
		return item, " has no prefix length — try " + item + "/24 or " + item + "/8."
	case strings.Contains(raw, "prefix length out of range"):
		ceiling := "/32"
		if strings.Contains(item, ":") {
			ceiling = "/128"
		}
		return item, " has a prefix over " + ceiling + "."
	case strings.Contains(raw, "bad bits after slash"):
		if j := strings.IndexByte(item, '/'); j >= 0 {
			item = item[:j]
		}
		return item, " has no prefix length — try " + item + "/24 or " + item + "/8."
	case strings.Contains(raw, "IPv4 address too short"), strings.Contains(raw, "IPv4 address too long"):
		return item, " needs four octets — like 192.168.0.0/16."
	case strings.Contains(item, ":"):
		return item, " isn't an IPv6 range — like fd00::/8."
	default:
		return item, " needs four octets — like 192.168.0.0/16."
	}
}
