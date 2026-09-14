package web

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// renderModal renders the modal component standalone (no page around it) with
// the given raw HTML as its children, the way any surface's @modal(...) { ... }
// call composes it in a .templ file.
func renderModal(t *testing.T, id, title, childHTML string) string {
	t.Helper()
	var buf bytes.Buffer
	ctx := templ.WithChildren(context.Background(), templ.Raw(childHTML))
	if err := modal(id, title).Render(ctx, &buf); err != nil {
		t.Fatalf("render modal: %v", err)
	}
	return buf.String()
}

// TestModalRendersHiddenAndAccessible asserts the modal component's a11y
// contract: it is a native <dialog>, closed by default (no `open` attribute, so
// it drops out of the tab order and a11y tree for free and stays out of the top
// layer until showModal), labelled by its own title, with a close control and
// the children slot carrying the caller's form content through unmodified.
func TestModalRendersHiddenAndAccessible(t *testing.T) {
	out := renderModal(t, "widgets-add", "Add a widget", `<p id="marker">form goes here</p>`)

	for _, want := range []string{
		`<dialog class="modal" id="widgets-add"`,
		`aria-labelledby="widgets-add-title"`,
		`id="widgets-add-title"`,
		`Add a widget`,
		`class="modal-body"`,
		`data-modal-dismiss`,
		`aria-label="Close"`,
		`id="marker"`,
		`form goes here`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("modal markup missing %q\ngot: %s", want, out)
		}
	}

	// Closed by default: a native dialog is hidden until showModal() unless it
	// carries `open`. It must not, or every modal would render open on the page.
	if strings.Contains(out, "open") {
		t.Errorf("modal must render closed (no open attribute): %s", out)
	}
	// The old hand-rolled overlay is gone — the top layer handles the backdrop.
	if strings.Contains(out, "modal-backdrop") || strings.Contains(out, "modal-panel") {
		t.Errorf("modal still carries the old overlay markup: %s", out)
	}
}

// TestAddButtonHelper asserts components.templ's addButton renders the
// data-modal-open trigger convention as the bar-size ghost verb — the app bar
// holds no filled accent (SPEC §2.6), so the header's Add verb is a ghost, not
// a primary fill. The visible text is the accessible name.
func TestAddButtonHelper(t *testing.T) {
	var buf bytes.Buffer
	if err := addButton("widgets-add", "Add widget").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render addButton: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`class="btn addbtn"`,
		`data-modal-open="widgets-add"`,
		`Add widget`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("addButton missing %q\ngot: %s", want, out)
		}
	}
	if strings.Contains(out, "aria-label") {
		t.Errorf("addButton carries a redundant aria-label (visible text is the name): %s", out)
	}
}

// TestModalInitWiredNative asserts modalInit() is wired once into the shared
// layout (so every page gets it for free) and that open/close reduce to the
// native <dialog> methods, with the delegated backdrop-click close. The
// element owns focus trapping, Esc, and scroll locking now — no hand-rolled
// keydown handler or panel-scoped focus query remains.
func TestModalInitWiredNative(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		"function exoModalOpen(id)",
		"function exoModalClose(id)",
		"m.showModal()",
		"m.close()",
		"data-modal-open",
		"data-modal-dismiss",
		// The backdrop region reports the dialog element as the click target.
		"e.target.tagName === 'DIALOG'",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("layout missing native modal wiring %q", want)
		}
	}
	// The hand-rolled trap and its keydown handler are gone.
	for _, gone := range []string{"exoModalKeyHandler", "panel.querySelectorAll("} {
		if strings.Contains(page, gone) {
			t.Errorf("layout still carries removed modal internals %q", gone)
		}
	}

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		".modal {", ".modal:not([open]) { display: none; }",
		".modal::backdrop", ".modal-close", ".addbtn", ".pageheader",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing modal rule %q", want)
		}
	}
	// The old overlay rules are gone.
	for _, gone := range []string{".modal-backdrop", ".modal-panel", `[data-modal-open] body`} {
		if strings.Contains(app, gone) {
			t.Errorf("app.css still carries removed modal rule %q", gone)
		}
	}
}
