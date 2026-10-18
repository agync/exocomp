package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// The source-scoped default egress set (Decision 0023) has a read face (the
// identity card's Egress ports line plus the CLI echo) and an operator write
// face (POST /sources/{name}/egress). These tests pin both.

// TestSourceDetailRendersEgressDefault: an operator-set default renders as the
// identity card's Egress ports line and echoes the CLI verb that records it, so
// the source's quiet otherwise-invisible egress widening is legible on the page.
func TestSourceDetailRendersEgressDefault(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/egress",
		url.Values{"egress": {"8080, 9000"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("set egress = %d, want 303", resp.StatusCode)
	}

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	for _, want := range []string{
		`<dt>Egress ports</dt><dd class="mono">8080, 9000</dd>`,
		"exocomp source egress " + name + " 8080 9000",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("source detail missing %q:\n%s", want, page)
		}
	}
}

// TestSourceDetailOmitsEmptyEgressDefault: a source with no operator default
// renders no Egress ports line — the quiet default posture. The operator egress
// card (whose field label also reads "Egress ports") is present, so the read
// value is asserted by its exact <dt> markup.
func TestSourceDetailOmitsEmptyEgressDefault(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	if strings.Contains(page, `<dt>Egress ports</dt>`) {
		t.Errorf("source detail shows an Egress ports line for an empty default:\n%s", page)
	}
}

// TestSourceEgressSet: the operator form's ports land on the source row
// (validated, deduped, sorted) and the verb redirects to the detail page.
func TestSourceEgressSet(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/egress",
		url.Values{"egress": {"9000,8080,9000"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("set egress = %d, want 303", resp.StatusCode)
	}
	src, err := h.store.SourceByName(context.Background(), name)
	if err != nil {
		t.Fatalf("SourceByName: %v", err)
	}
	if !slices.Equal(src.ExtraEgressPorts, []int{8080, 9000}) {
		t.Fatalf("ExtraEgressPorts = %v, want [8080 9000] (sorted, deduped)", src.ExtraEgressPorts)
	}
}

// TestSourceEgressClear: an empty field clears the declaration — the same
// no-ports form as the CLI's `source egress <name>`.
func TestSourceEgressClear(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/egress",
		url.Values{"egress": {"8080"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("set egress = %d, want 303", resp.StatusCode)
	}

	resp = postForm(t, client, ts.URL+"/sources/"+name+"/egress",
		url.Values{"egress": {""}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear egress = %d, want 303", resp.StatusCode)
	}
	src, err := h.store.SourceByName(context.Background(), name)
	if err != nil {
		t.Fatalf("SourceByName: %v", err)
	}
	if len(src.ExtraEgressPorts) != 0 {
		t.Fatalf("ExtraEgressPorts = %v after clear, want empty", src.ExtraEgressPorts)
	}
}

// TestSourceEgressRefusesJunk: a junk or out-of-range port re-renders the detail
// page at 422 with the egress refusal and the typed value kept, and mutates
// nothing — the forms pattern, never a 500 and never a silent drop.
func TestSourceEgressRefusesJunk(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, egress := range []string{"0", "70000", "abc"} {
		resp := postForm(t, client, ts.URL+"/sources/"+name+"/egress",
			url.Values{"egress": {egress}}, nil)
		body := readBody(t, resp)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("egress=%s = %d, want 422 (%s)", egress, resp.StatusCode, body)
		}
		if !strings.Contains(body, "Egress ports are numbers from 1 to 65535") {
			t.Errorf("egress=%s refusal missing the field sentence:\n%s", egress, body)
		}
		if !strings.Contains(body, `value="`+egress+`"`) {
			t.Errorf("egress=%s refusal did not keep the typed value:\n%s", egress, body)
		}
	}
	src, err := h.store.SourceByName(context.Background(), name)
	if err != nil {
		t.Fatalf("SourceByName: %v", err)
	}
	if len(src.ExtraEgressPorts) != 0 {
		t.Fatalf("a junk egress set still landed %v", src.ExtraEgressPorts)
	}
}

// TestSourceEgressUnknownSource: an unknown source is a 404, like every other
// source-scoped verb — never a silent create.
func TestSourceEgressUnknownSource(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/sources/nope/egress",
		url.Values{"egress": {"2222"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-source egress = %d, want 404", resp.StatusCode)
	}
}

// TestSourceEgressRefusesUnauthenticatedAndUnderScoped: the verb sits behind the
// same operator floor as the rest of the surface — no session redirects to
// /login, a readonly token is forbidden, and neither mutates the source.
func TestSourceEgressRefusesUnauthenticatedAndUnderScoped(t *testing.T) {
	ctx := context.Background()

	t.Run("no session is refused and mutates nothing", func(t *testing.T) {
		h := newHarness(t)
		ts := httptest.NewServer(h.server.Handler())
		defer ts.Close()
		name := seedSourceCheckout(t, h)

		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := client.PostForm(ts.URL+"/sources/"+name+"/egress", url.Values{"egress": {"2222"}})
		if err != nil {
			t.Fatalf("POST egress: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("unauthenticated egress = %d, want 303 to /login", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Fatalf("unauthenticated egress redirected to %q, want /login…", loc)
		}
		src, err := h.store.SourceByName(ctx, name)
		if err != nil {
			t.Fatalf("SourceByName: %v", err)
		}
		if len(src.ExtraEgressPorts) != 0 {
			t.Fatalf("unauthenticated POST set %v", src.ExtraEgressPorts)
		}
	})

	t.Run("readonly scope is forbidden and mutates nothing", func(t *testing.T) {
		h := newHarness(t)
		ts := httptest.NewServer(h.server.Handler())
		defer ts.Close()
		name := seedSourceCheckout(t, h)
		client := login(t, ts.URL, h.mint(t, "viewer", "readonly"))

		resp := postForm(t, client, ts.URL+"/sources/"+name+"/egress",
			url.Values{"egress": {"2222"}}, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("readonly egress = %d, want 403", resp.StatusCode)
		}
		src, err := h.store.SourceByName(ctx, name)
		if err != nil {
			t.Fatalf("SourceByName: %v", err)
		}
		if len(src.ExtraEgressPorts) != 0 {
			t.Fatalf("readonly POST set %v", src.ExtraEgressPorts)
		}
	})
}
