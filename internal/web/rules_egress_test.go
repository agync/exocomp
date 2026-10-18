package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// These tests pin Decision 0023's last web surface: the rules page must render
// a playbook rule's EFFECTIVE egress — its declared ports unioned with the
// default set of the source it names, merged at queue admission — so a rule
// that declares nothing can never read as "no egress guard" while its
// unattended runs open a port.

// seedEgressSource registers a path source, optionally with an operator-set
// default egress set. An empty set leaves the source at the port-22 posture.
func seedEgressSource(t *testing.T, h *harness, name string, ports ...int) {
	t.Helper()
	ctx := context.Background()
	src, err := h.store.CreateSource(ctx, store.Source{
		Name: name, Kind: store.SourcePath, URI: "/srv/" + name, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateSource %s: %v", name, err)
	}
	if len(ports) == 0 {
		return
	}
	if _, err := h.store.SetSourceEgressPorts(ctx, src.ID, ports); err != nil {
		t.Fatalf("SetSourceEgressPorts %s: %v", name, err)
	}
}

// seedEgressRule registers a rule through the store (the rules list is
// CLI-managed, so the store is the only seam) with the given action kind,
// action source, and declared egress set.
func seedEgressRule(t *testing.T, h *harness, name, kind, source string, egress []int) {
	t.Helper()
	if _, err := h.store.CreateRule(context.Background(), store.NewRule{
		Name: name, CELExpr: "true", ActionKind: kind,
		Action: store.RuleAction{Source: source, Playbook: "site.yml", Egress: egress},
	}); err != nil {
		t.Fatalf("CreateRule %s: %v", name, err)
	}
}

// rulesEgress resolves every rule row's rendered egress guard by name, the
// same read the /rules handler performs.
func rulesEgress(t *testing.T, h *harness) map[string]string {
	t.Helper()
	rows, err := h.server.rules(httptest.NewRequest("GET", "/rules", nil))
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Name] = r.Egress
	}
	return out
}

// (a) A playbook rule naming a source with a default renders the source's set
// even though the rule declares nothing — the gap the rules page had. Before
// the fix this read as no guard while the run opened 443.
func TestRulesEgressRendersSourceDefault(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	seedEgressSource(t, h, "api", 443)
	seedEgressRule(t, h, "api-call", store.ActionPlaybook, "api", nil)

	if got := rulesEgress(t, h)["api-call"]; got != "egress 443" {
		t.Fatalf("rule egress = %q, want %q (inherited from the source default)", got, "egress 443")
	}

	// The surface an operator reads carries the same guard.
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, client, ts.URL+"/rules")
	if code != 200 {
		t.Fatalf("/rules = %d, want 200", code)
	}
	if !strings.Contains(page, `playbook api/site.yml`) {
		t.Fatal("/rules missing the playbook action phrase")
	}
	if !strings.Contains(page, "egress 443") {
		t.Error("/rules must render the source-inherited egress guard")
	}
}

// (b) Declared ports plus a source default render the deduped, sorted union.
func TestRulesEgressUnionsDeclaredAndSourceDefault(t *testing.T) {
	h := newHarness(t)
	seedEgressSource(t, h, "edge", 9090, 443)
	seedEgressRule(t, h, "converge", store.ActionPlaybook, "edge", []int{2222, 9090})

	if got := rulesEgress(t, h)["converge"]; got != "egress 443, 2222, 9090" {
		t.Fatalf("rule egress = %q, want the deduped union %q", got, "egress 443, 2222, 9090")
	}
}

// (c) A playbook rule naming no source renders only its declared set.
func TestRulesEgressNoSourceDeclaredOnly(t *testing.T) {
	h := newHarness(t)
	seedEgressSource(t, h, "edge", 443)
	seedEgressRule(t, h, "local", store.ActionPlaybook, "", []int{2222})

	if got := rulesEgress(t, h)["local"]; got != "egress 2222" {
		t.Fatalf("rule egress = %q, want the declared set only %q", got, "egress 2222")
	}
}

// (d) A playbook rule whose source has no default renders only its declared
// set — the source widens nothing.
func TestRulesEgressSourceWithoutDefault(t *testing.T) {
	h := newHarness(t)
	seedEgressSource(t, h, "plain")
	seedEgressRule(t, h, "declared", store.ActionPlaybook, "plain", []int{2222})
	seedEgressRule(t, h, "bare", store.ActionPlaybook, "plain", nil)

	got := rulesEgress(t, h)
	if got["declared"] != "egress 2222" {
		t.Fatalf("rule egress = %q, want %q", got["declared"], "egress 2222")
	}
	if got["bare"] != "" {
		t.Fatalf("rule egress = %q, want empty (no declaration, no default)", got["bare"])
	}
}

// (e) A missing source neither errors nor widens: the declared set stands, and
// a rule that declares nothing stays guard-less.
func TestRulesEgressMissingSourceNoOp(t *testing.T) {
	h := newHarness(t)
	seedEgressRule(t, h, "declared", store.ActionPlaybook, "ghost", []int{2222})
	seedEgressRule(t, h, "bare", store.ActionPlaybook, "ghost", nil)

	got := rulesEgress(t, h) // a lookup error here would fail the test
	if got["declared"] != "egress 2222" {
		t.Fatalf("rule egress = %q, want %q (declared set preserved)", got["declared"], "egress 2222")
	}
	if got["bare"] != "" {
		t.Fatalf("rule egress = %q, want empty (a missing source is a no-op)", got["bare"])
	}
}

// (f) A non-playbook rule is unchanged: even one carrying a source name and a
// declared set renders no guard, because only an ansible run is widened.
func TestRulesEgressNonPlaybookUnchanged(t *testing.T) {
	h := newHarness(t)
	seedEgressSource(t, h, "api", 443)
	seedEgressRule(t, h, "restart", store.ActionShell, "api", []int{2222})

	if got := rulesEgress(t, h)["restart"]; got != "" {
		t.Fatalf("shell rule egress = %q, want empty (non-playbook rules render no guard)", got)
	}
}
