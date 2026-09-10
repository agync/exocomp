package web

// Row actions (v0.6): a row shows at most two verb glyphs — the actions an
// operator reaches for daily on that resource — then a chevron that opens a
// menu of the rarer verbs and views. Fewer than three actions total → glyphs
// only, no chevron. Every glyph is a 24-viewBox inline stroke SVG
// (stroke-width 1.8, currentColor, round caps) with an aria-label — never a
// glyph-only control (components.templ). Menu items are always glyph + word;
// destructive items live last, after a rule, in --signal-fail with an
// "asks first" note. The trailing "…" is retired: a button that opens a
// confirm is named plainly and the confirm dialog is the safety.

// rowAction is one row action: a glyph (its path + accessible name) plus the
// behaviour — a POST form target, a modal id to open, or a plain link — and
// optionally a menu-only flag. The first two non-menu actions render as the
// row's verb glyphs; the rest (and any menu-only action) go in the chevron
// menu. A glyph that POSTs is a form submit; a glyph that opens a modal is a
// data-modal-open trigger.
type rowAction struct {
	Name   string            // accessible name / tooltip text
	Path   string            // 24-viewBox stroke path
	Form   string            // non-empty: a POST to this action
	Hidden map[string]string // hidden inputs for the POST (confirmed, etc.)
	// Confirm is the confirm-dialog message (the shared confirm dialog is the
	// safety for destructive actions); empty means no confirm.
	Confirm string
	Modal   string // non-empty: opens this dialog id
	Href    string // non-empty: a plain link
	Count   string // optional right-aligned mono count
	Menu    bool   // force this action into the menu, never a row glyph
	// Danger places the item last, in --signal-fail, with "asks first".
	Danger bool
}

// The per-verb glyph set — the same shape means the same verb on every
// resource (the designer's fixed-per-verb rule).
const (
	glyphPlan     = "M12 5v14M5 12h14"
	glyphApply    = "M5 12l4 4L19 6"
	glyphEnable   = "M7 5v14l10-7z"
	glyphDisable  = "M6 6l12 12M18 6L6 18"
	glyphOpen     = "M14 4h6v6M10 14l10-10M19 13v7H4V5h7"
	glyphRotate   = "M4 10a8 8 0 0 1 13.7-2.7M20 14a8 8 0 0 1-13.7 2.7M16 4h2v4h-4"
	glyphRemove   = "M6 7h12M9 7V5h6v2M8 10l.6 9h6.8l.6-9M10 13v3M14 13v3"
	glyphBindings = "M4 6h16v9H4zM7 18h10M12 15v3"
	glyphChevron  = "M6 9l6 6 6-6"
	// glyphDocument is the Raw log verb (T6): a file with folded corner. The
	// designer's document path (row-actions/GLYPHS.txt) adds to the shipped
	// set rather than forking it.
	glyphDocument = "M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8l-5-5zM14 3v5h5"
	glyphTail     = "M5 12h14M12 5l7 7-7 7"
	// glyphDiff is the plan-diff verb (T3): two bars with offset ticks, the
	// designer's diff path verbatim. Every plan row deep-links it to the plan
	// detail page (lane F owns the route).
	glyphDiff = "M8 4v16M16 4v16M5 8h6M13 16h6"
	// glyphReveal is the backend reveal verb (T4): an eye with a pupil, the
	// designer's reveal path. It shows the bearer beside the block for 30s and
	// logs the reveal; the pupil is a separate circle in the SVG.
	glyphReveal = "M3 12s3.5-6 9-6 9 6 9 6-3.5 6-9 6-9-6-9-6Z"
)
