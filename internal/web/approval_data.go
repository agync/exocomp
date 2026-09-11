package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// approvalVM builds the phone approval surface for one approval, rendering the
// seven-point contract. A decided or expired approval resolves to a terminal
// notice; a pending one carries its fail-closed countdown, its plan (destroys
// first), its provenance, and — only when the workspace serial moved — the
// stale-plan refusal.
func (s *Server) approvalVM(r *http.Request, id int64, _ string) (approvalVM, error) {
	a, err := s.store.GetApproval(r.Context(), id)
	if err != nil {
		return approvalVM{}, err
	}
	now := s.now()

	vm := approvalVM{
		Page:      s.pageFor(r, "Approvals", "approvals"),
		ID:        a.ID,
		JobID:     a.JobID,
		Workspace: a.Workspace,
	}
	if job, err := s.store.GetJob(r.Context(), a.JobID); err == nil {
		vm.Runbook = job.Runbook
		vm.Kind = job.Kind
		vm.Provenance = fmt.Sprintf("Job %d (%s) was gated before it can apply; it was triggered %s.",
			job.ID, job.Runbook, job.Trigger)
		// The triggering event names the rule that raised the gate: only the
		// normalized name/source/rule, never the raw event body.
		if ev, err := s.store.EventByJobID(r.Context(), job.ID); err == nil && ev.MatchedRule != "" {
			vm.Provenance = fmt.Sprintf("Job %d (%s) was gated after %s event %s matched rule %s.",
				job.ID, job.Runbook, ev.Source, wire.EventName(ev), ev.MatchedRule)
		}
	}

	ident, _ := identityOf(r.Context())

	switch {
	case a.State != store.ApprovalPending:
		vm.Decided = true
		vm.Outcome = string(a.State)
		vm.Notice = decidedNotice(a)
		return vm, nil
	case !now.Before(a.ExpiresAt):
		vm.Expired = true
		return vm, nil
	}

	remaining := a.ExpiresAt.Sub(now)
	vm.Countdown = humanAge(remaining) + " left"
	vm.DeadlineMS = a.ExpiresAt.UnixMilli()
	vm.CanDecide = mayApprove(ident)

	// Stale-plan is a refusal, not a disabled button: if the workspace moved
	// past the plan's serial, the operator re-plans or discards.
	if a.Workspace != "" {
		ws, err := s.store.WorkspaceByName(r.Context(), a.Workspace)
		if err == nil && ws.Serial > a.PlanSerial {
			vm.Stale = true
			vm.StaleNotice = fmt.Sprintf(
				"The plan was made at serial %d and %s is now at serial %d. The plan is kept but cannot be applied — re-plan, or discard it. Nothing was applied.",
				a.PlanSerial, ws.Name, ws.Serial)
		}
	}

	if a.PlanArtifact != nil && s.plans != nil && !vm.Stale {
		destroys, changes, ok := s.renderPlan(r, *a.PlanArtifact)
		if ok {
			vm.HasPlan = true
			vm.Destroys = destroys
			vm.Changes = changes
		}
	}
	return vm, nil
}

// planShowBlob resolves the JSON a plan's diff renders from. A plan produced
// by this control plane carries a sealed `tofu show -json` companion artifact
// (the executor captured it at plan time, while the run env was live), which is
// the authoritative source: the saved plan file tofu wrote is a binary, not a
// show-json document, and re-running tofu show later cannot be hermetic. A plan
// with no companion — one sealed before the capture existed, or an operator's
// own plan file — falls back to the plan artifact's own bytes, the pre-capture
// behaviour. Read-only sessions are gated by the caller.
func (s *Server) planShowBlob(r *http.Request, art store.Artifact) ([]byte, error) {
	if art.JobID != 0 {
		if arts, err := s.store.ListArtifacts(r.Context(), art.JobID); err == nil {
			for _, a := range arts {
				if a.Kind == reactive.PlanShowArtifactKind {
					return s.plans.OpenSealedFile(a.Path)
				}
			}
		}
	}
	return s.plans.OpenSealedFile(art.Path)
}

// renderPlan reads a sealed plan artifact through the envelope — never the raw
// ciphertext — and splits it into the lines that destroy (rendered first, in
// red) and the rest. Secret-backed attributes are marked redacted, never
// carried with their value. The classification and redaction come from the
// shared classifyPlan core, so the browser plan view and the /api/v1 plan diff
// cannot diverge; renderPlan keeps only the fields the HTML template consumes.
func (s *Server) renderPlan(r *http.Request, artifactID int64) (destroys, changes []planLine, ok bool) {
	art, err := s.store.GetArtifact(r.Context(), artifactID)
	if err != nil {
		return nil, nil, false
	}
	blob, err := s.planShowBlob(r, art)
	if err != nil {
		s.log.Warn("web: open sealed plan", "artifact", artifactID, "err", err)
		return nil, nil, false
	}
	resources, ok := classifyPlan(blob)
	if !ok {
		return nil, nil, false
	}
	for _, rc := range resources {
		// classifyPlan now keeps no-op/read rows for the plan-detail page;
		// the phone approval never shows a row it is not asking about.
		if rc.Action == "no-op" || rc.Action == "read" {
			continue
		}
		line := planLine{Action: rc.Action, Address: rc.Address, Detail: rc.Action, Redacted: rc.Redacted}
		if rc.Action == "delete" || rc.Action == "replace" {
			destroys = append(destroys, line)
		} else {
			changes = append(changes, line)
		}
	}
	return destroys, changes, true
}

// planResource is one classified, redacted resource line of a sealed plan: the
// projection shared by the web approval surface (renderPlan), the /api/v1 plan
// endpoint (apiApprovalPlan), and the plan-detail page (SPEC §4). Redacted
// marks a resource whose sealed plan flags anything secret; its Attrs then
// carry redactedValue in place of every attribute value, so a secret-backed
// value is never carried with its plaintext.
//
// Attrs is the FLATTENED leaf set (SPEC §4.1): map leaves carry dotted paths
// (data.marker), list elements an index path (metadata[0]), and an unchanged
// map/slice subtree collapses to one entry carrying its key count. Changed
// and Same count on that same flattened set, so the plan-detail row cell and
// footer always agree (B3).
type planResource struct {
	Action   string
	Address  string
	Redacted bool
	Attrs    []planAttr
}

// planAttr is one attribute row with its before and after values. Before is
// empty when the attribute is new, After when it was removed; Same is set when
// the value did not move (the "N attributes unchanged" collapse). Both values
// are redactedValue, never the plaintext, whenever the owning resource is
// redacted. Same is decided by reflect.DeepEqual on the decoded values before
// redaction, so a redacted resource still reports its true change count.
//
// Shape is the value cell's render shape (SPEC §4.1): inline for a short
// scalar change, block for a longer scalar, object for a map/slice value.
// BeforeDisplay/AfterDisplay are the display renderer's output (quoted
// strings, bare bools/numbers) the inline/block cells show; an object attr
// carries its key-by-key Object diff and RawBefore/RawAfter minified pair
// instead. ObjectKeys is the key count of an UNCHANGED object attr ("1 block ·
// N keys · unchanged", 0 for an empty map rendering {}).
type planAttr struct {
	Name          string
	Before        string
	After         string
	Redacted      bool
	Same          bool
	Shape         attrShape
	BeforeDisplay string
	AfterDisplay  string
	Object        *objectDiff
	ObjectKeys    int
}

// attrShape is the value cell's render shape (SPEC §4.1): inline when the
// combined display length is <= 48 chars, block for a longer scalar, object
// for a map/slice value.
type attrShape string

const (
	attrShapeInline attrShape = "inline"
	attrShapeBlock  attrShape = "block"
	attrShapeObject attrShape = "object"
)

// objectDiff is the key-by-key diff of an object-shaped attribute — the
// rendered lines the plan-detail <pre> shows (SPEC §4.1 B5). KeysTotal is K:
// the number of non-server-set keys at the object's top level; KeysChanged
// counts the top-level keys whose value moved; server-set keys count in
// neither. RawBefore/RawAfter are the minified before/after pair the raw
// drawer opens.
type objectDiff struct {
	Lines       []objectLine
	KeysTotal   int
	KeysChanged int
	RawBefore   string
	RawAfter    string
}

// objectLine is one rendered line of an object diff. Indent is the
// pre-rendered 2-space depth. Key is the quoted key with the column padding
// applied (and the - / + sign for a changed scalar) so values align across a
// depth; Value is the display-rendered value; Status drives the line's tint:
// "same" | "del" | "add" | "server-set" | "secret".
type objectLine struct {
	Indent string
	Key    string
	Value  string
	Status string
}

// redactedValue is the marker shown in place of a secret-backed value, the same
// text the web plan view renders (view_approval.templ planDetail).
const redactedValue = "[redacted · secret]"

// serverSetFields are the known-after-apply resource keys that are not a
// change when tofu reports them (SPEC §4.1): the provider stamps them on
// apply, so a plan that differs only here is a no-op. The list is the
// growable extension point for any other provider-set field. At the top
// level they are dropped entirely; inside an object diff they render their
// server-set placeholder.
var serverSetFields = []string{"uid", "resource_version"}

func isServerSet(name string) bool {
	for _, f := range serverSetFields {
		if name == f {
			return true
		}
	}
	return false
}

// ChangedAttrs are the flattened changed leaves of this resource, in the
// order planAttrs emitted them; UnchangedAttrs the flattened unchanged set
// (scalars at their leaf path, unchanged objects collapsed to one entry). The
// plan-detail template loops ONLY the changed set (SPEC §4.1 B1) and the row
// cell and the footer count the same two lists, so they always agree (B3).
func (r planResource) ChangedAttrs() []planAttr {
	var out []planAttr
	for _, at := range r.Attrs {
		if !at.Same {
			out = append(out, at)
		}
	}
	return out
}

func (r planResource) UnchangedAttrs() []planAttr {
	var out []planAttr
	for _, at := range r.Attrs {
		if at.Same {
			out = append(out, at)
		}
	}
	return out
}

// Changed is the number of attributes whose value moved (Before != After);
// Same the rest. The plan detail's per-resource "N changed · M same" reads
// them. Both count the FLATTENED Attrs set through the same two lists the
// template's changed loop and the unchanged footer iterate, so the row cell
// and the footer can never disagree.
func (r planResource) Changed() int { return len(r.ChangedAttrs()) }

func (r planResource) Same() int { return len(r.UnchangedAttrs()) }

// ActionWord is the keyword the diff table's ACTION cell shows (CREATE · UPDATE
// · REPLACE · DESTROY). The default is the NO-OP row the plan-detail page
// renders unchanged — the phone and /api/v1 never show it.
func (r planResource) ActionWord() string {
	switch r.Action {
	case "create":
		return "CREATE"
	case "update":
		return "UPDATE"
	case "replace":
		return "REPLACE"
	case "delete":
		return "DESTROY"
	default:
		return "NO-OP"
	}
}

// ActionClass is the keyword's signal: create ok, update/replace wait, delete
// fail. Never colour-only — the keyword word is always shown.
func (r planResource) ActionClass() string {
	switch r.Action {
	case "create":
		return "ok"
	case "update", "replace":
		return "wait"
	case "delete":
		return "fail"
	default:
		return "dim"
	}
}

// classifyPlan parses a decrypted tofu plan blob into the ordered, classified,
// redacted resource list both surfaces build from. It is the one place the
// sensitivity decision (hasSensitive over the *_sensitive trees) and the action
// classification (classifyActions) live. no-op and read rows SURVIVE here —
// SPEC §4.1 shows them on the plan-detail page as unchanged — and the phone
// (renderPlan) and the /api/v1 wire view (apiApprovalPlan) filter them out of
// their buckets. A blob that will not parse yields ok=false — the same soft
// failure renderPlan returned.
func classifyPlan(blob []byte) (resources []planResource, ok bool) {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions         []string        `json:"actions"`
				Before          map[string]any  `json:"before"`
				After           map[string]any  `json:"after"`
				AfterSensitive  json.RawMessage `json:"after_sensitive"`
				BeforeSensitive json.RawMessage `json:"before_sensitive"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(blob, &plan); err != nil {
		return nil, false
	}
	for _, rc := range plan.ResourceChanges {
		action := classifyActions(rc.Change.Actions)
		redacted := hasSensitive(rc.Change.AfterSensitive) || hasSensitive(rc.Change.BeforeSensitive)
		resources = append(resources, planResource{
			Action:   action,
			Address:  rc.Address,
			Redacted: redacted,
			Attrs:    planAttrs(rc.Change.Before, rc.Change.After, redacted),
		})
	}
	return resources, true
}

// planAttrs projects a resource's before/after attribute maps into the
// FLATTENED leaf rows the plan detail renders (SPEC §4.1). The key set is the
// union of both maps, so a created attribute has an empty Before and a removed
// one an empty After; server-set keys are dropped entirely at the top level.
// A map subtree recurses to leaf rows at dotted paths (data.marker); a list to
// element rows at index paths (metadata[0]); an unchanged map/slice subtree
// collapses to one object entry carrying its key count ("1 block · N keys ·
// unchanged", or "{}" when empty). Same is decided with valuesEqual on the raw
// values before redaction, so a redacted resource still reports its true
// change count. When the resource is redacted every value becomes
// redactedValue — the coarse, resource-granular decision hasSensitive already
// makes for the whole line — so a secret-backed value never appears with its
// plaintext.
func planAttrs(before, after map[string]any, redacted bool) []planAttr {
	keys := objectKeys(before, after)
	if len(keys) == 0 {
		return nil
	}
	var out []planAttr
	for _, name := range keys {
		out = append(out, flattenValue(name, before[name], after[name], redacted)...)
	}
	return out
}

// flattenValue reduces one before/after pair at a dotted path into the
// flattened rows: maps recurse to leaves, slices to index paths, a scalar (or
// a type change to a map/slice) to one leaf row.
func flattenValue(path string, b, a any, redacted bool) []planAttr {
	bm, bIsMap := b.(map[string]any)
	am, aIsMap := a.(map[string]any)
	if bIsMap && aIsMap {
		return flattenObject(path, bm, am, redacted)
	}
	bs, bIsSlice := b.([]any)
	as, aIsSlice := a.([]any)
	if bIsSlice && aIsSlice {
		return flattenSlice(path, bs, as, redacted)
	}
	return []planAttr{leafAttr(path, b, a, redacted)}
}

// flattenObject recurses a map subtree. A subtree with no differing leaf
// collapses to one unchanged object entry; one holding only server-set keys
// contributes nothing (never a change, never in the unchanged list). A subtree
// with a differing leaf surfaces each leaf at its dotted path.
func flattenObject(path string, b, a map[string]any, redacted bool) []planAttr {
	keys := objectKeys(b, a)
	if len(keys) == 0 {
		if len(b) == 0 && len(a) == 0 {
			return []planAttr{unchangedObject(path, 0, redacted)}
		}
		return nil
	}
	if valuesEqual(b, a) {
		return []planAttr{unchangedObject(path, len(keys), redacted)}
	}
	var out []planAttr
	for _, k := range keys {
		out = append(out, flattenValue(path+"."+k, b[k], a[k], redacted)...)
	}
	return out
}

// flattenSlice recurses a list by index path. An element that is an object on
// both sides is an OBJECT leaf at its index path (SCREENS 2b renders the whole
// element as the object pre); an element that is itself a list recurses; a
// scalar element is a leaf row. An unchanged list collapses to one unchanged
// object entry.
func flattenSlice(path string, b, a []any, redacted bool) []planAttr {
	if valuesEqual(b, a) {
		return []planAttr{unchangedObject(path, maxi(len(b), len(a)), redacted)}
	}
	var out []planAttr
	for i := 0; i < maxi(len(b), len(a)); i++ {
		out = append(out, flattenSliceElement(path+"["+strconv.Itoa(i)+"]", sliceAt(b, i), sliceAt(a, i), redacted)...)
	}
	return out
}

// flattenSliceElement classifies one list element pair. An object element is
// the object leaf (changed) or an unchanged object entry; a nested list
// recurses; a scalar element is a leaf row.
func flattenSliceElement(path string, b, a any, redacted bool) []planAttr {
	bm, bIsMap := b.(map[string]any)
	am, aIsMap := a.(map[string]any)
	if bIsMap && aIsMap {
		if valuesEqual(b, a) {
			return []planAttr{unchangedObject(path, len(objectKeys(bm, am)), redacted)}
		}
		return []planAttr{objectAttr(path, b, a, redacted)}
	}
	bs, bIsSlice := b.([]any)
	as, aIsSlice := a.([]any)
	if bIsSlice && aIsSlice {
		return flattenSlice(path, bs, as, redacted)
	}
	return []planAttr{leafAttr(path, b, a, redacted)}
}

// leafAttr is one scalar row. The shape is object when the value is a
// map/slice on either side (a type change); otherwise inline when the combined
// display length is <= 48 chars (quotes counted), else block. For a redacted
// resource every display field is the marker, regardless of shape.
func leafAttr(path string, b, a any, redacted bool) planAttr {
	at := planAttr{Name: path, Before: attrValue(b), After: attrValue(a), Same: valuesEqual(b, a), Redacted: redacted}
	_, bObj := b.(map[string]any)
	_, aObj := a.(map[string]any)
	_, bSlice := b.([]any)
	_, aSlice := a.([]any)
	if bObj || aObj || bSlice || aSlice {
		at.Shape = attrShapeObject
		at.Object = buildObjectDiff(path, b, a, redacted)
	} else {
		bd, ad := displayValue(b), displayValue(a)
		at.BeforeDisplay, at.AfterDisplay = bd, ad
		if at.Same || len(bd)+len(ad) <= 48 {
			at.Shape = attrShapeInline
		} else {
			at.Shape = attrShapeBlock
		}
	}
	if redacted {
		at.Before, at.After = redactedValue, redactedValue
		at.BeforeDisplay, at.AfterDisplay = redactedValue, redactedValue
	}
	return at
}

// objectAttr is a changed object leaf (a list element object, or a type
// change): the object shape with its key-by-key diff.
func objectAttr(path string, b, a any, redacted bool) planAttr {
	at := planAttr{Name: path, Before: attrValue(b), After: attrValue(a), Redacted: redacted, Shape: attrShapeObject}
	at.Object = buildObjectDiff(path, b, a, redacted)
	if redacted {
		at.Before, at.After = redactedValue, redactedValue
		at.BeforeDisplay, at.AfterDisplay = redactedValue, redactedValue
	}
	return at
}

// unchangedObject is the collapsed unchanged object entry: Same with its
// ObjectKeys for "1 block · N keys · unchanged" (0 renders "{}").
func unchangedObject(path string, keys int, redacted bool) planAttr {
	at := planAttr{Name: path, Same: true, Shape: attrShapeObject, ObjectKeys: keys}
	if redacted {
		at.BeforeDisplay, at.AfterDisplay = redactedValue, redactedValue
	}
	return at
}

// valuesEqual is the canonical before/after compare (SPEC §4.1 B2):
// reflect.DeepEqual on the decoded values, map-order-independent, so identical
// maps never false-diff. When DeepEqual differs, the key space is re-walked to
// forgive the server-set fields, which are "not changes" no matter where they
// sit; nested maps and lists compare elementwise the same way.
func valuesEqual(b, a any) bool {
	if reflect.DeepEqual(b, a) {
		return true
	}
	bm, bIsMap := b.(map[string]any)
	am, aIsMap := a.(map[string]any)
	if bIsMap && aIsMap {
		for _, k := range unionKeys(bm, am) {
			if isServerSet(k) {
				continue
			}
			if !valuesEqual(bm[k], am[k]) {
				return false
			}
		}
		return true
	}
	bs, bIsSlice := b.([]any)
	as, aIsSlice := a.([]any)
	if bIsSlice && aIsSlice {
		for i := 0; i < maxi(len(bs), len(as)); i++ {
			if !valuesEqual(sliceAt(bs, i), sliceAt(as, i)) {
				return false
			}
		}
		return true
	}
	return false
}

// buildObjectDiff renders the object diff for an object-shaped attr. When
// both sides are maps the diff is key-by-key over the sorted union: a changed
// scalar becomes a - / + pair, a changed nested map with a shared key recurses
// (indent + 2), a wholesale map replacement (no shared key) and a type change
// become the - / + pair of the whole values. Server-set keys render their
// placeholder and count in neither K nor the changed count. A redacted
// resource renders every line as the secret marker (secret precedence over
// server-set) while K and the changed count stay the true pre-redaction ones.
func buildObjectDiff(path string, b, a any, redacted bool) *objectDiff {
	diff := objectDiff{RawBefore: attrValue(b), RawAfter: attrValue(a)}
	bm, bIsMap := b.(map[string]any)
	am, aIsMap := a.(map[string]any)
	if bIsMap && aIsMap {
		lines, total, changed := objectLines(bm, am, redacted, 0)
		diff.Lines, diff.KeysTotal, diff.KeysChanged = lines, total, changed
		return &diff
	}
	if redacted {
		diff.Lines = []objectLine{{Key: strconv.Quote(path) + ":", Value: redactedValue, Status: "secret"}}
	} else {
		diff.Lines = []objectLine{
			{Key: "- " + strconv.Quote(path) + ":", Value: objectValue(b), Status: "del"},
			{Key: "+ " + strconv.Quote(path) + ":", Value: objectValue(a), Status: "add"},
		}
	}
	diff.KeysTotal = 1
	diff.KeysChanged = 1
	return &diff
}

// objectLineRow is one staged object-diff row before its column padding is
// applied; Nested carries a map pair that recurses at the next depth.
type objectLineRow struct {
	Sign   string
	Key    string
	Value  string
	Status string
	Nested bool
	B, A   any
}

// objectLines builds the rendered object-diff lines for a map pair at one
// depth, then pads every key to the longest at that depth so values align.
// KeysTotal and KeysChanged count the TOP-LEVEL non-server-set keys and the
// ones whose value moved; a recursed nested map counts as one changed key
// here, its own leaves do not roll up.
func objectLines(bm, am map[string]any, redacted bool, depth int) (lines []objectLine, keysTotal, keysChanged int) {
	var rows []objectLineRow
	for _, k := range unionKeys(bm, am) {
		var bv, av any
		if _, ok := bm[k]; ok {
			bv = bm[k]
		}
		if _, ok := am[k]; ok {
			av = am[k]
		}
		if redacted {
			rows = append(rows, objectLineRow{Key: k, Value: redactedValue, Status: "secret"})
			if !isServerSet(k) {
				keysTotal++
				if !valuesEqual(bv, av) {
					keysChanged++
				}
			}
			continue
		}
		if isServerSet(k) {
			rows = append(rows, objectLineRow{Key: k, Value: "[redacted · server-set]", Status: "server-set"})
			continue
		}
		keysTotal++
		if valuesEqual(bv, av) {
			rows = append(rows, objectLineRow{Key: k, Value: objectValue(bv), Status: "same"})
			continue
		}
		keysChanged++
		bm2, b2Map := bv.(map[string]any)
		am2, a2Map := av.(map[string]any)
		if b2Map && a2Map && len(bm2) > 0 && len(am2) > 0 && shareKey(bm2, am2) {
			rows = append(rows, objectLineRow{Key: k, Nested: true, B: bv, A: av})
			continue
		}
		rows = append(rows, objectLineRow{Sign: "- ", Key: k, Value: objectValue(bv), Status: "del"})
		rows = append(rows, objectLineRow{Sign: "+ ", Key: k, Value: objectValue(av), Status: "add"})
	}
	width := 0
	for _, r := range rows {
		if r.Nested {
			continue
		}
		if w := len(r.Sign) + len(strconv.Quote(r.Key)) + 1; w > width {
			width = w
		}
	}
	indent := strings.Repeat("  ", depth)
	var out []objectLine
	for _, r := range rows {
		if r.Nested {
			bm2, _ := r.B.(map[string]any)
			am2, _ := r.A.(map[string]any)
			sub, _, _ := objectLines(bm2, am2, redacted, depth+1)
			out = append(out, sub...)
			continue
		}
		key := r.Sign + strconv.Quote(r.Key) + ":" + strings.Repeat(" ", width-len(r.Sign)-len(strconv.Quote(r.Key))-1)
		out = append(out, objectLine{Indent: indent, Key: key, Value: r.Value, Status: r.Status})
	}
	return out, keysTotal, keysChanged
}

// objectValue renders one object-diff value: a scalar through the display
// renderer (quoted), a map or slice as its compact JSON.
func objectValue(v any) string {
	if _, isMap := v.(map[string]any); isMap {
		return attrValue(v)
	}
	if _, isSlice := v.([]any); isSlice {
		return attrValue(v)
	}
	return displayValue(v)
}

// displayValue renders a scalar for the inline/block shapes (SPEC §4.1):
// strings quoted, booleans and numbers bare, nil as "null". It measures the
// 48-char inline threshold on ITS output (quotes counted). Maps and slices
// never reach it — a changed structured value is the object shape.
func displayValue(v any) string {
	switch t := v.(type) {
	case string:
		return strconv.Quote(t)
	case bool:
		return fmt.Sprint(t)
	case float64:
		return fmt.Sprintf("%g", t)
	case nil:
		return "null"
	default:
		return ""
	}
}

// objectKeys is a map pair's sorted non-server-set key union — the K rule's
// key space for flattening and the unchanged object count.
func objectKeys(b, a map[string]any) []string {
	keys := unionKeys(b, a)
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if !isServerSet(k) {
			out = append(out, k)
		}
	}
	return out
}

// unionKeys is a map pair's sorted key union.
func unionKeys(b, a map[string]any) []string {
	seen := map[string]struct{}{}
	for k := range b {
		seen[k] = struct{}{}
	}
	for k := range a {
		seen[k] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// shareKey reports whether two maps hold at least one common key — the
// signal that a nested map pair is a structural diff worth recursing into
// rather than a wholesale replacement.
func shareKey(x, y map[string]any) bool {
	for k := range x {
		if _, ok := y[k]; ok {
			return true
		}
	}
	return false
}

func sliceAt(s []any, i int) any {
	if i < len(s) {
		return s[i]
	}
	return nil
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// attrValue renders a non-redacted attribute value as a compact string: a
// string verbatim, a null as empty, anything structured as its JSON encoding.
func attrValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// classifyActions maps a tofu action set to one risk-ordered word.
func classifyActions(actions []string) string {
	has := func(a string) bool {
		for _, x := range actions {
			if x == a {
				return true
			}
		}
		return false
	}
	switch {
	case has("delete") && has("create"):
		return "replace"
	case has("delete"):
		return "delete"
	case has("create"):
		return "create"
	case has("update"):
		return "update"
	case has("read"):
		return "read"
	default:
		return "no-op"
	}
}

// hasSensitive reports whether a plan's *_sensitive tree marks anything secret.
// It delegates to the shared classifier (wire.HasSensitivePlan) so the web,
// TUI, API, and CLI read one decision, never a per-face fork.
func hasSensitive(raw json.RawMessage) bool {
	return wire.HasSensitivePlan(raw)
}

func decidedNotice(a store.Approval) string {
	when := ""
	if a.DecidedAt != nil {
		when = " at " + a.DecidedAt.Format("2006-01-02 15:04:05Z")
	}
	switch a.State {
	case store.ApprovalApproved:
		return fmt.Sprintf("Approved%s. The job is queued and will run. Nothing else to do here.", when)
	case store.ApprovalRejected:
		return fmt.Sprintf("Rejected%s. The job was cancelled; nothing was applied.", when)
	case store.ApprovalExpired:
		return "The decision window closed with no answer. The job was rejected and nothing was applied."
	case store.ApprovalWithdrawn:
		return "The condition cleared before a decision, so this was withdrawn and its job cancelled."
	default:
		return fmt.Sprintf("This decision is settled (%s). Nothing changed.", a.State)
	}
}
