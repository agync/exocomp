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
// contract: closed by default via the native hidden attribute (not just a CSS
// class, so it drops out of the tab order and a11y tree for free), the dialog
// role/aria-modal/aria-labelledby wiring, a close control, and the children
// slot carrying the caller's form content through unmodified.
func TestModalRendersHiddenAndAccessible(t *testing.T) {
	out := renderModal(t, "widgets-add", "Add a widget", `<p id="marker">form goes here</p>`)

	for _, want := range []string{
		`id="widgets-add"`,
		` hidden`,
		`role="dialog"`,
		`aria-modal="true"`,
		`aria-labelledby="widgets-add-title"`,
		`id="widgets-add-title"`,
		`Add a widget`,
		`data-modal-dismiss`,
		`aria-label="Close"`,
		`id="marker"`,
		`form goes here`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("modal markup missing %q\ngot: %s", want, out)
		}
	}

	// hidden must land on the outer .modal container, not just the panel —
	// otherwise the backdrop would still paint over the page while "closed".
	openIdx := strings.Index(out, `class="modal"`)
	hiddenIdx := strings.Index(out, ` hidden`)
	panelIdx := strings.Index(out, `class="modal-panel"`)
	inOrder := openIdx != -1 && hiddenIdx != -1 && panelIdx != -1 && openIdx < hiddenIdx && hiddenIdx < panelIdx
	if !inOrder {
		t.Errorf("hidden attribute is not on the outer .modal container: %s", out)
	}
}

// TestAddButtonHelper asserts components.templ's addButton renders the
// data-modal-open trigger convention with an accessible name, since the glyph
// itself carries no visible text.
func TestAddButtonHelper(t *testing.T) {
	var buf bytes.Buffer
	if err := addButton("widgets-add", "widget").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render addButton: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`class="addbtn"`,
		`data-modal-open="widgets-add"`,
		`aria-label="Add widget"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("addButton missing %q\ngot: %s", want, out)
		}
	}
}

// TestModalInitWiredAndFocusTrapScoped asserts modalInit() is wired once into
// the shared layout (so every page gets it for free) and that its focus trap
// re-queries focusable elements scoped to the open .modal-panel — never the
// whole document — so a rowact <details> dropdown elsewhere on the page (e.g.
// Secrets'/Tokens' rotate controls) can never be caught by a modal's trap.
func TestModalInitWiredAndFocusTrapScoped(t *testing.T) {
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
		"data-modal-open",
		"data-modal-dismiss",
		`document.addEventListener("keydown", exoModalKeyHandler)`,
		// The trap query is rooted at panel (the open .modal-panel), not
		// document — so a <details> dropdown elsewhere on the page (e.g.
		// Secrets'/Tokens' rowact rotate controls) is never caught by a
		// modal's own trap.
		"panel.querySelectorAll(",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("layout missing modalInit wiring %q", want)
		}
	}

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		".modal {", ".modal[hidden] { display: none; }",
		".modal-backdrop", ".modal-panel", ".modal-close", ".addbtn",
		".pageheader", `[data-modal-open] body { overflow: hidden; }`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing modal rule %q", want)
		}
	}
}
