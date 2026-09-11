package web

import (
	"reflect"
	"strings"
	"testing"
)

// plan_attrs_test.go is the pure-engine spec for the plan-diff flattening
// (SPEC §4.1, wave-1 engine). It drives classifyPlan/planAttrs directly — no
// server harness — and pins the semantics lane B (the template) builds the
// three value shapes on: reflect.DeepEqual comparison, leaf flattening,
// server-set exclusion, the object K rule, no-op/read retention, and the
// flattened changed/unchanged sets the row cell and footer both count.

// classify returns the one resource of a single-resource plan, failing the
// test if it does not parse to exactly one resource.
func classify(t *testing.T, planJSON string) planResource {
	resources, ok := classifyPlan([]byte(planJSON))
	if !ok {
		t.Fatalf("classifyPlan rejected %s", planJSON)
	}
	if len(resources) != 1 {
		t.Fatalf("resources = %d, want 1 (%s)", len(resources), planJSON)
	}
	return resources[0]
}

// attrOf finds the attribute named name, failing the test when absent.
func attrOf(t *testing.T, rc planResource, name string) planAttr {
	for _, at := range rc.Attrs {
		if at.Name == name {
			return at
		}
	}
	t.Fatalf("no attr %q in %+v", name, rc.Attrs)
	return planAttr{}
}

// TestPlanDiffIdenticalMapsUnchanged is the B2 false-diff regression: an
// IDENTICAL nested map — with its JSON keys in a different order on each side
// — must classify unchanged, because the compare is reflect.DeepEqual on the
// decoded values (map order-independent), never attrValue-string equality. A
// top-level map holding only server-set keys is dropped entirely: it is
// never a change and never in the unchanged list.
func TestPlanDiffIdenticalMapsUnchanged(t *testing.T) {
	rc := classify(t, `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["update"],
		"before":{"metadata":{"a":1,"b":2},"data":{"uid":"u","resource_version":"r"}},
		"after":{"metadata":{"b":2,"a":1},"data":{"resource_version":"r","uid":"u"}}}}]}`)
	if rc.Changed() != 0 || rc.Same() != 1 {
		t.Fatalf("changed/same = %d/%d, want 0/1", rc.Changed(), rc.Same())
	}
	if len(rc.Attrs) != 1 || rc.Attrs[0].Name != "metadata" {
		t.Fatalf("attrs = %+v, want only the unchanged metadata object", rc.Attrs)
	}
	md := attrOf(t, rc, "metadata")
	if !md.Same || md.Shape != attrShapeObject || md.ObjectKeys != 2 {
		t.Fatalf("metadata = %+v, want an unchanged object with 2 keys", md)
	}
	for _, at := range rc.Attrs {
		if at.Name == "data" {
			t.Fatalf("server-set-only map surfaced as an attr: %+v", at)
		}
	}
	if len(rc.ChangedAttrs()) != rc.Changed() || len(rc.UnchangedAttrs()) != rc.Same() {
		t.Fatalf("row cell and footer disagree: %d/%d vs %d/%d",
			len(rc.ChangedAttrs()), len(rc.UnchangedAttrs()), rc.Changed(), rc.Same())
	}
}

// TestPlanDiffNullVsEmptyChanged pins B2's DeepEqual semantics where the
// attrValue-string compare lies: null renders "" and so does the empty
// string, but they are a REAL change. Mutation check: swap valuesEqual for
// `attrValue(before) == attrValue(after)` and this goes red.
func TestPlanDiffNullVsEmptyChanged(t *testing.T) {
	rc := classify(t, `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["update"],"before":{"k":null},"after":{"k":""}}}]}`)
	if rc.Changed() != 1 || rc.Same() != 0 {
		t.Fatalf("changed/same = %d/%d, want 1/0 (null -> \"\" is a change)", rc.Changed(), rc.Same())
	}
	k := attrOf(t, rc, "k")
	if k.Same {
		t.Fatalf("null -> \"\" classified same: %+v", k)
	}
}

// TestPlanDiffServerSetExcluded: uid and resource_version are "not changes"
// (SPEC §4.1) — a moving uid never counts, and a map whose ONLY differences
// are server-set keys surfaces nothing at all, not even as unchanged.
func TestPlanDiffServerSetExcluded(t *testing.T) {
	rc := classify(t, `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["update"],
		"before":{"uid":"a","keep":"same","metadata":{"uid":"u1","resource_version":"r1"}},
		"after":{"uid":"b","keep":"same","metadata":{"uid":"u2","resource_version":"r2"}}}}]}`)
	if rc.Changed() != 0 {
		t.Fatalf("changed = %d, want 0 (a moving uid is not a change)", rc.Changed())
	}
	if len(rc.Attrs) != 1 || rc.Attrs[0].Name != "keep" {
		t.Fatalf("attrs = %+v, want only the unchanged keep leaf", rc.Attrs)
	}
	keep := attrOf(t, rc, "keep")
	if !keep.Same || keep.Shape != attrShapeInline || keep.BeforeDisplay != `"same"` {
		t.Fatalf("keep = %+v, want an unchanged scalar with quoted display", keep)
	}
}

// TestPlanDiffLeafFlattening pins the dotted-path leaf rows (review fold 1):
// the changed leaf is data.marker, the unchanged leaf data.keep surfaces in
// the unchanged set, and `data` itself is never a row.
func TestPlanDiffLeafFlattening(t *testing.T) {
	rc := classify(t, `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["update"],
		"before":{"data":{"marker":"DRIFTED","keep":"same"}},
		"after":{"data":{"marker":"hello-exocomp","keep":"same"}}}}]}`)
	if rc.Changed() != 1 || rc.Same() != 1 {
		t.Fatalf("changed/same = %d/%d, want 1/1", rc.Changed(), rc.Same())
	}
	changed := rc.ChangedAttrs()
	if len(changed) != 1 || changed[0].Name != "data.marker" {
		t.Fatalf("changed = %+v, want the data.marker leaf", changed)
	}
	marker := changed[0]
	if marker.Same || marker.Shape != attrShapeInline {
		t.Fatalf("data.marker = %+v, want a changed inline leaf", marker)
	}
	if marker.Before != "DRIFTED" || marker.After != "hello-exocomp" {
		t.Fatalf("data.marker pair = %q -> %q, want DRIFTED -> hello-exocomp", marker.Before, marker.After)
	}
	if marker.BeforeDisplay != `"DRIFTED"` || marker.AfterDisplay != `"hello-exocomp"` {
		t.Fatalf("data.marker display = %q -> %q, want the quoted pair", marker.BeforeDisplay, marker.AfterDisplay)
	}
	same := rc.UnchangedAttrs()
	if len(same) != 1 || same[0].Name != "data.keep" {
		t.Fatalf("unchanged = %+v, want the data.keep leaf", same)
	}
	if !same[0].Same || same[0].Shape != attrShapeInline || same[0].BeforeDisplay != `"same"` {
		t.Fatalf("data.keep = %+v, want an unchanged scalar with quoted display", same[0])
	}
}

// TestPlanDiffShapeClassification pins the three shapes: inline when the
// combined display length is <= 48 (quotes counted on the display renderer's
// output), block for a longer scalar, object for a value whose type changed
// scalar -> map.
func TestPlanDiffShapeClassification(t *testing.T) {
	rc := classify(t, `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["update"],
		"before":{"tag":"old","notes":"provisioned by exocomp job #2 against serial 0 · lima k3s · owner platform-team · do not edit by hand","blob":"x"},
		"after":{"tag":"new","notes":"provisioned by exocomp job #4 against serial 1 · lima k3s · owner platform-team · do not edit by hand · drift restored 19:30","blob":{"a":1}}}}]}`)
	tag := attrOf(t, rc, "tag")
	if tag.Shape != attrShapeInline || len(tag.BeforeDisplay)+len(tag.AfterDisplay) > 48 {
		t.Fatalf("tag = %+v, want inline (combined %d <= 48)",
			tag, len(tag.BeforeDisplay)+len(tag.AfterDisplay))
	}
	if tag.BeforeDisplay != `"old"` || tag.AfterDisplay != `"new"` {
		t.Fatalf("tag display = %q -> %q, want quoted old -> new", tag.BeforeDisplay, tag.AfterDisplay)
	}
	notes := attrOf(t, rc, "notes")
	if notes.Shape != attrShapeBlock || len(notes.BeforeDisplay)+len(notes.AfterDisplay) <= 48 {
		t.Fatalf("notes = %+v, want block (combined %d > 48)",
			notes, len(notes.BeforeDisplay)+len(notes.AfterDisplay))
	}
	if !strings.HasPrefix(notes.BeforeDisplay, `"provisioned by exocomp job #2`) || !strings.HasSuffix(notes.BeforeDisplay, `"`) {
		t.Fatalf("notes before display = %q, want the quoted long string", notes.BeforeDisplay)
	}
	blob := attrOf(t, rc, "blob")
	if blob.Shape != attrShapeObject || blob.Object == nil {
		t.Fatalf("blob = %+v, want an object shape with a diff", blob)
	}
	if blob.Object.KeysTotal != 1 || blob.Object.KeysChanged != 1 {
		t.Fatalf("blob object counts = %d/%d, want 1/1", blob.Object.KeysTotal, blob.Object.KeysChanged)
	}
	if len(blob.Object.Lines) != 2 ||
		blob.Object.Lines[0].Status != "del" || blob.Object.Lines[0].Value != `"x"` ||
		blob.Object.Lines[1].Status != "add" || blob.Object.Lines[1].Value != `{"a":1}` {
		t.Fatalf("blob object lines = %+v, want the -/ + pair", blob.Object.Lines)
	}
	if !strings.HasPrefix(blob.Object.Lines[0].Key, `- "blob":`) ||
		!strings.HasPrefix(blob.Object.Lines[1].Key, `+ "blob":`) {
		t.Fatalf("blob object keys = %q / %q, want the quoted path with sign",
			blob.Object.Lines[0].Key, blob.Object.Lines[1].Key)
	}
}

// TestPlanDiffObjectKRule pins the object K rule (review fold 2): for a
// changed map object the key count K excludes the server-set keys, which
// render as their server-set placeholder and never count as changed. The raw
// pair is the minified before/after of the whole object.
func TestPlanDiffObjectKRule(t *testing.T) {
	rc := classify(t, `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["update"],
		"before":{"metadata":[{"a":1,"uid":"u1","resource_version":"r1"}]},
		"after":{"metadata":[{"a":2,"uid":"u1","resource_version":"r1"}]}}}]}`)
	if rc.Changed() != 1 || rc.Same() != 0 {
		t.Fatalf("changed/same = %d/%d, want 1/0", rc.Changed(), rc.Same())
	}
	obj := attrOf(t, rc, "metadata[0]")
	if obj.Same || obj.Shape != attrShapeObject || obj.Object == nil {
		t.Fatalf("metadata[0] = %+v, want a changed object with a diff", obj)
	}
	od := obj.Object
	if od.KeysTotal != 1 || od.KeysChanged != 1 {
		t.Fatalf("object counts = %d/%d, want K=1 (a) with 1 changed (server-set excluded)",
			od.KeysTotal, od.KeysChanged)
	}
	if od.RawBefore != `{"a":1,"resource_version":"r1","uid":"u1"}` ||
		od.RawAfter != `{"a":2,"resource_version":"r1","uid":"u1"}` {
		t.Fatalf("raw pair = %s -> %s, want the minified before/after", od.RawBefore, od.RawAfter)
	}
	var aLine, uidLine *objectLine
	for i := range od.Lines {
		if od.Lines[i].Status == "del" && strings.Contains(od.Lines[i].Key, `"a"`) {
			aLine = &od.Lines[i]
		}
		if strings.Contains(od.Lines[i].Key, `"uid"`) {
			uidLine = &od.Lines[i]
		}
	}
	if aLine == nil || aLine.Status != "del" || aLine.Value != "1" {
		t.Fatalf("no del line for a=1: %+v", od.Lines)
	}
	if uidLine == nil || uidLine.Status != "server-set" || uidLine.Value != "[redacted · server-set]" {
		t.Fatalf("uid line = %+v, want the server-set placeholder", uidLine)
	}
	if len(od.Lines) != 4 {
		t.Fatalf("object lines = %d, want 4 (a -/+ pair plus two server-set lines)", len(od.Lines))
	}
}

// TestPlanDiffNoOpReadKept: classifyPlan KEEPS no-op and read rows (SPEC §4.1
// shows them unchanged); the downstream surfaces filter them, never the
// classifier. Mutation check: restore the old `continue` in classifyPlan and
// this goes red.
func TestPlanDiffNoOpReadKept(t *testing.T) {
	resources, ok := classifyPlan([]byte(`{"resource_changes":[
		{"address":"null_resource.idle","change":{"actions":["no-op"],"before":{"a":1},"after":{"a":1}}},
		{"address":"null_resource.ro","change":{"actions":["read"]}},
		{"address":"null_resource.upd","change":{"actions":["update"],"before":{"b":1},"after":{"b":2}}}
	]}`))
	if !ok {
		t.Fatalf("classifyPlan rejected a plan with no-op/read rows")
	}
	if len(resources) != 3 {
		t.Fatalf("resources = %d, want 3 (no-op and read survive)", len(resources))
	}
	if resources[0].Action != "no-op" || resources[1].Action != "read" || resources[2].Action != "update" {
		t.Fatalf("actions = %q / %q / %q, want no-op / read / update",
			resources[0].Action, resources[1].Action, resources[2].Action)
	}
	if resources[0].Changed() != 0 || resources[0].Same() != 1 {
		t.Fatalf("no-op row counts = %d/%d, want 0/1", resources[0].Changed(), resources[0].Same())
	}
	if a := attrOf(t, resources[0], "a"); !a.Same || a.BeforeDisplay != "1" {
		t.Fatalf("no-op attr a = %+v, want an unchanged bare number", a)
	}
}

// TestPlanDiffRedactionPreserved: a resource whose after_sensitive tree flags
// anything secret keeps every value as the redaction marker — the true
// pre-redaction comparison still drives the changed/unchanged counts.
func TestPlanDiffRedactionPreserved(t *testing.T) {
	rc := classify(t, `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["update"],
		"before":{"secret":"hunter2","keep":"k"},
		"after":{"secret":"hunter3","keep":"k"},
		"after_sensitive":{"secret":true}}}]}`)
	if !rc.Redacted {
		t.Fatalf("resource not flagged redacted")
	}
	secret := attrOf(t, rc, "secret")
	if secret.Same || secret.Before != redactedValue || secret.After != redactedValue ||
		secret.BeforeDisplay != redactedValue || secret.AfterDisplay != redactedValue {
		t.Fatalf("secret = %+v, want the marker everywhere with a true change", secret)
	}
	keep := attrOf(t, rc, "keep")
	if !keep.Same || keep.Before != redactedValue || keep.BeforeDisplay != redactedValue {
		t.Fatalf("keep = %+v, want the marker even when unchanged", keep)
	}
	if rc.Changed() != 1 || rc.Same() != 1 {
		t.Fatalf("changed/same = %d/%d, want 1/1 (true comparison)", rc.Changed(), rc.Same())
	}
	for _, at := range rc.Attrs {
		if strings.Contains(at.Before, "hunter") || strings.Contains(at.After, "hunter") {
			t.Fatalf("plaintext leaked into %+v", at)
		}
	}
}

// TestPlanDiffRedactedObjectSecrets: a secret-redacted object diff renders
// every line as the secret marker (secret precedence over server-set), while
// the key counts stay the true pre-redaction ones.
func TestPlanDiffRedactedObjectSecrets(t *testing.T) {
	rc := classify(t, `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["update"],
		"before":{"metadata":[{"a":1,"uid":"u"}]},
		"after":{"metadata":[{"a":2,"uid":"u"}]},
		"after_sensitive":{"metadata":true}}}]}`)
	obj := attrOf(t, rc, "metadata[0]")
	if !rc.Redacted || obj.Shape != attrShapeObject || obj.Object == nil {
		t.Fatalf("metadata[0] = %+v, want a redacted object diff", obj)
	}
	od := obj.Object
	for _, ln := range od.Lines {
		if ln.Status != "secret" || ln.Value != redactedValue {
			t.Fatalf("redacted object line = %+v, want the secret marker", ln)
		}
	}
	if od.KeysTotal != 1 || od.KeysChanged != 1 {
		t.Fatalf("redacted object counts = %d/%d, want the true 1/1", od.KeysTotal, od.KeysChanged)
	}
}

// TestPlanDiffChangedSameCounts pins the SCREENS 2a row: data.marker changed
// with binary_data / id / immutable / metadata unchanged -> 1 changed · 4
// same. The row cell and the footer read the SAME flattened sets (B3); the
// unchanged object collapses to one entry carrying its key count.
func TestPlanDiffChangedSameCounts(t *testing.T) {
	rc := classify(t, `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["update"],
		"before":{"data":{"marker":"DRIFTED"},"binary_data":{},"id":"exocomp-e2e/e2e-marker","immutable":false,"metadata":{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7}},
		"after":{"data":{"marker":"hello-exocomp"},"binary_data":{},"id":"exocomp-e2e/e2e-marker","immutable":false,"metadata":{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7}}}}]}`)
	if rc.Changed() != 1 || rc.Same() != 4 {
		t.Fatalf("changed/same = %d/%d, want 1/4", rc.Changed(), rc.Same())
	}
	changed := rc.ChangedAttrs()
	if len(changed) != 1 || changed[0].Name != "data.marker" {
		t.Fatalf("changed = %+v, want only data.marker", changed)
	}
	unchanged := rc.UnchangedAttrs()
	if len(unchanged) != 4 {
		t.Fatalf("unchanged = %+v, want 4 entries", unchanged)
	}
	names := make([]string, 0, len(unchanged))
	for _, at := range unchanged {
		names = append(names, at.Name)
	}
	if want := []string{"binary_data", "id", "immutable", "metadata"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("unchanged names = %+v, want %+v", names, want)
	}
	if md := attrOf(t, rc, "metadata"); md.Same != true || md.Shape != attrShapeObject || md.ObjectKeys != 7 {
		t.Fatalf("metadata = %+v, want 1 block · 7 keys · unchanged", md)
	}
	if bd := attrOf(t, rc, "binary_data"); bd.Same != true || bd.Shape != attrShapeObject || bd.ObjectKeys != 0 {
		t.Fatalf("binary_data = %+v, want the {} entry", bd)
	}
	if imm := attrOf(t, rc, "immutable"); imm.Same != true || imm.BeforeDisplay != "false" {
		t.Fatalf("immutable = %+v, want a bare false", imm)
	}
	if id := attrOf(t, rc, "id"); id.Same != true || id.BeforeDisplay != `"exocomp-e2e/e2e-marker"` {
		t.Fatalf("id = %+v, want the quoted id", id)
	}
	if len(rc.ChangedAttrs()) != rc.Changed() || len(rc.UnchangedAttrs()) != rc.Same() {
		t.Fatalf("row cell and footer disagree: %d/%d vs %d/%d",
			len(rc.ChangedAttrs()), len(rc.UnchangedAttrs()), rc.Changed(), rc.Same())
	}
}
