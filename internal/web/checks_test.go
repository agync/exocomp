package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// seedTCPCheck registers a tcp check with two targets and writes each one's
// state row — laptop up, nas down — so the page can render both statuses.
func seedTCPCheck(t *testing.T, h *harness) store.Check {
	t.Helper()
	ctx := context.Background()
	c, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: "192.168.1.50", Port: 22},
			{Name: "nas", Address: "192.168.1.60", Port: 22},
		},
	})
	if err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	now := time.Now().UTC()
	for _, tc := range []struct {
		target, status string
	}{
		{"laptop", store.CheckUp},
		{"nas", store.CheckDown},
	} {
		if err := h.store.SetCheckState(ctx, store.CheckState{
			CheckID: c.ID, TargetName: tc.target, Status: tc.status,
			Consecutive: 3, Emitted: tc.status, FirstSeen: now, LastChange: now,
		}); err != nil {
			t.Fatalf("SetCheckState %s: %v", tc.target, err)
		}
	}
	return c
}

// TestChecksSurfaceGated is the gate for the checks view: it is token-gated
// (no session refused, session renders), it shows the check with each
// target's last observed status, the nav carries Checks, and an operator sees
// the add/enable/disable/rm write controls while a readonly session sees
// none.
func TestChecksSurfaceGated(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedTCPCheck(t, h)

	// No session: refused with a redirect to the login form.
	noSession := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noSession.Get(ts.URL + "/checks")
	if err != nil {
		t.Fatalf("GET /checks no session: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/checks without session = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("/checks redirect = %q, want /login…", loc)
	}

	// Operator: the surface renders with the check, per-target statuses, and
	// the write controls.
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks with session = %d, want 200", code)
	}
	for _, want := range []string{
		"at-home", "tcp", "5m0s",
		"laptop", "nas", "192.168.1.50:22",
		`<span class="chip ok"><span class="dot"></span>UP</span>`,
		`<span class="chip fail"><span class="dot"></span>DOWN</span>`,
		`action="/checks/add"`, `/checks/at-home/disable`, `/checks/at-home/rm`,
		`href="/checks" aria-current="page"`,
		"nothing fires until a rule matches", `href="/rules">/rules</a>`,
		`class="checkcard"`, `class="targetgrid"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks (operator) missing %q", want)
		}
	}

	// Readonly: the page renders, without any write control.
	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))
	code, page = getBody(t, ro, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks readonly = %d, want 200", code)
	}
	if !strings.Contains(page, "Read-only: managing checks needs an operator token.") {
		t.Error("/checks (readonly) missing the read-only note")
	}
	for _, banned := range []string{`action="/checks/add"`, `/checks/at-home/update`, `/checks/at-home/disable`, `/checks/at-home/rm`} {
		if strings.Contains(page, banned) {
			t.Errorf("/checks (readonly) leaked write control %q", banned)
		}
	}
}

// TestChecksNavElsewhere: the Checks nav item appears on other pages too, so
// it is reachable from anywhere in the surface.
func TestChecksNavElsewhere(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/")
	if !strings.Contains(page, `href="/checks"`) || !strings.Contains(page, ">Checks</span>") {
		t.Error("dashboard nav missing the Checks link")
	}
}

// TestCheckAddCreates drives the add form end to end: repeated target fields
// parse into two tcp targets and the row lands enabled. Kind tcp dials only
// on the sweep, so no live network is touched at create time (unlike ping).
func TestCheckAddCreates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{
			"name":     {"at-home"},
			"kind":     {store.CheckTCP},
			"interval": {"5m"},
			"target":   {"laptop=192.168.1.50:22", "nas=192.168.1.60:22"},
		}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("check add = %d, want 303", resp.StatusCode)
	}

	c, err := h.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if c.Kind != store.CheckTCP {
		t.Fatalf("kind = %q, want tcp", c.Kind)
	}
	if len(c.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(c.Targets))
	}
	if !c.Enabled {
		t.Fatal("newly created check should be enabled")
	}
}

// TestCheckAddHTTPExpect drives the http add path with a pinned expected
// status, so the expect field lands on every target.
func TestCheckAddHTTPExpect(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{
			"name":     {"web-health"},
			"kind":     {store.CheckHTTP},
			"interval": {"1m"},
			"target":   {"web=https://web.lan/health"},
			"expect":   {"200"},
		}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("http check add = %d, want 303", resp.StatusCode)
	}

	c, err := h.store.GetCheck(ctx, "web-health")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if len(c.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(c.Targets))
	}
	if c.Targets[0].URL != "https://web.lan/health" {
		t.Fatalf("target url = %q, want https://web.lan/health", c.Targets[0].URL)
	}
	if c.Targets[0].Expect != 200 {
		t.Fatalf("target expect = %d, want 200", c.Targets[0].Expect)
	}
}

// TestCheckAddKinds drives the new kinds through the add form: a tls target
// stores host:port and its min-days floor, a dns target stores its expected
// address, and a metric target stores its operator and value.
func TestCheckAddKinds(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		values url.Values
		check  func(t *testing.T, c store.Check)
	}{
		{
			name: "cert",
			values: url.Values{
				"name": {"cert"}, "kind": {store.CheckTLS}, "interval": {"1h"},
				"target": {"web=web.lan:443"}, "min-days": {"21"},
			},
			check: func(t *testing.T, c store.Check) {
				if c.Targets[0].Address != "web.lan" || c.Targets[0].Port != 443 || c.Targets[0].MinDays != 21 {
					t.Fatalf("tls target = %+v, want web.lan:443 min_days 21", c.Targets[0])
				}
			},
		},
		{
			name: "resolve",
			values: url.Values{
				"name": {"resolve"}, "kind": {store.CheckDNS}, "interval": {"1h"},
				"target": {"api=api.lan"}, "expect-addr": {"192.168.1.10"},
			},
			check: func(t *testing.T, c store.Check) {
				if c.Targets[0].DNSName != "api.lan" || c.Targets[0].ExpectAddr != "192.168.1.10" {
					t.Fatalf("dns target = %+v, want api.lan -> 192.168.1.10", c.Targets[0])
				}
			},
		},
		{
			name: "depth",
			values: url.Values{
				"name": {"depth"}, "kind": {store.CheckMetric}, "interval": {"1h"},
				"target": {"depth=http://nas:9090/queue"}, "op": {">="}, "value": {"100"},
			},
			check: func(t *testing.T, c store.Check) {
				if c.Targets[0].URL != "http://nas:9090/queue" || c.Targets[0].Op != ">=" || c.Targets[0].Value != 100 {
					t.Fatalf("metric target = %+v, want op >= value 100", c.Targets[0])
				}
			},
		},
	} {
		resp := postForm(t, op, ts.URL+"/checks/add", tc.values, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s add = %d, want 303", tc.name, resp.StatusCode)
		}
		c, err := h.store.GetCheck(ctx, tc.name)
		if err != nil {
			t.Fatalf("%s GetCheck: %v", tc.name, err)
		}
		tc.check(t, c)
	}
}

// TestCheckAddKindRefusals: a kind-specific field set on the wrong kind, and a
// metric missing its operator, refuse on the field's own meta line at 422 and
// store nothing.
func TestCheckAddKindRefusals(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{
			"name": {"depth"}, "kind": {store.CheckMetric}, "interval": {"1h"},
			"target": {"depth=http://nas:9090/queue"}, "op": {"~"}, "value": {"100"},
		}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad metric op status = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{`aria-invalid="true"`, "The operator is one of", `value="~"`} {
		if !strings.Contains(body, want) {
			t.Errorf("metric op refusal missing %q", want)
		}
	}
	if _, err := h.store.GetCheck(context.Background(), "depth"); err == nil {
		t.Fatal("a bad metric op still stored a check")
	}

	// min-days on a tcp check pins a tls target only.
	resp = postForm(t, op, ts.URL+"/checks/add",
		url.Values{
			"name": {"wrong"}, "kind": {store.CheckTCP}, "interval": {"1h"},
			"target": {"a=192.168.1.50:22"}, "min-days": {"21"},
		}, nil)
	body = readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("min-days on tcp status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Min-days pins a tls target only.") {
		t.Errorf("min-days kind refusal missing its sentence")
	}
	if _, err := h.store.GetCheck(context.Background(), "wrong"); err == nil {
		t.Fatal("a mismatched kind field still stored a check")
	}
}

// TestCheckEditRendersStoredKindFields: a stored tls check's edit dialog
// prefills the stored min-days and does not render the http expect field.
func TestCheckEditRendersStoredKindFields(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "cert", Kind: store.CheckTLS, IntervalS: 3600,
		Targets: []store.CheckTarget{{Name: "web", Address: "web.lan", Port: 443, MinDays: 21}},
	}); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks = %d, want 200", code)
	}
	for _, want := range []string{`id="cke-cert-min-days"`, `value="21"`} {
		if !strings.Contains(page, want) {
			t.Errorf("tls edit dialog missing %q", want)
		}
	}
	if strings.Contains(page, `id="cke-cert-expect"`) {
		t.Error("tls edit dialog rendered the http expect field")
	}
}

// flashMessage extracts the flash a rejected form redirect carries, asserting
// the redirect went back to wantPath. Flash now travels in a server-set
// HttpOnly cookie (never a URL parameter), so the message is read off the
// 303's Set-Cookie header. The message is the operator's one-line error; a
// caller asserts a stable substring of it.
func flashMessage(t *testing.T, resp *http.Response, wantPath string) string {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	// The redirect target may carry its own query (e.g. /checks?tab=netgroups);
	// compare the path portion, not the whole Location.
	locPath, _, _ := strings.Cut(loc, "?")
	if locPath != wantPath {
		t.Fatalf("redirect = %q, want %s", loc, wantPath)
	}
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie {
			return c.Value
		}
	}
	t.Fatalf("redirect to %q carries no flash cookie", wantPath)
	return ""
}

// TestCheckFormAtRest renders the checks-add dialog for an operator and
// asserts the forms-round anatomy: the reusable field pattern with
// always-present meta lines (help at rest), the kind select's own meta line,
// the repeated target group's shared meta, aria wiring tying each control to
// its meta, a form-status line, and novalidate so the daemon stays
// authoritative.
func TestCheckFormAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks = %d, want 200", code)
	}
	for _, want := range []string{
		`id="checks-add"`,
		`action="/checks/add"`,
		`id="check-add-form" novalidate`,
		`aria-describedby="ck-name-meta"`,
		`aria-describedby="ck-kind-meta"`,
		`aria-describedby="ck-interval-meta"`,
		`aria-describedby="ck-target-meta"`,
		`aria-describedby="ck-expect-meta"`,
		`aria-describedby="ck-min-days-meta"`,
		`aria-describedby="ck-expect-addr-meta"`,
		`aria-describedby="ck-op-meta"`,
		`aria-describedby="ck-value-meta"`,
		`id="ck-name-meta"`,
		`id="ck-kind-meta"`,
		`id="ck-interval-meta"`,
		`id="ck-target-meta"`,
		`id="ck-expect-meta"`,
		`id="ck-min-days-meta"`,
		`id="ck-expect-addr-meta"`,
		`id="ck-op-meta"`,
		`id="ck-value-meta"`,
		"Letters, digits, dots, hyphens, underscores.",
		"Kind is tcp, ping, http, dns, tls or metric.",
		"A Go duration — 5m, 90s, 1h.",
		"Targets are name=spec",
		"A whole status code, like 200 or 404.",
		"Whole days until the leaf cert expires, like 21.",
		"Require the name to resolve to this address.",
		"One of &lt;, &lt;=, &gt;, &gt;=, ==, !=.",
		"The number the metric body is compared against.",
		`role="status" aria-live="polite"`,
		"Add check",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("check form missing %q", want)
		}
	}
	// The kind select offers all six kinds.
	for _, k := range []string{"tcp", "ping", "http", "dns", "tls", "metric"} {
		want := `<option value="` + k + `"`
		if !strings.Contains(page, want) {
			t.Errorf("kind select missing %q", want)
		}
	}
	// The four baseline target rows are the JS-off baseline.
	for _, want := range []string{
		`placeholder="laptop=192.168.1.50:22"`,
		`placeholder="nas=192.168.1.60:22"`,
		`placeholder="web=https://web.lan/health"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("check form missing baseline target row %q", want)
		}
	}
	// At rest no control carries a refusal and the metas are plain help text.
	if !strings.Contains(page, `placeholder="at-home" aria-describedby="ck-name-meta">`) {
		t.Error("at-rest name control carries an aria-invalid refusal")
	}
	if !strings.Contains(page, `id="ck-name-meta" class="meta">Letters`) {
		t.Error("at-rest name meta is not plain help text")
	}
}

// TestCheckFormRefusalsRender asserts the 422 re-render fills every meta line
// at once (name, kind, interval, target, expect), keeps the typed values, and
// never flashes: a refusal is the form's own surface, flash is success-only.
func TestCheckFormRefusalsRender(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{
			"name": {"bad name!"}, "kind": {store.CheckTCP}, "interval": {"abc"},
			"target": {"laptop"}, "expect": {"x"},
		}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("refusal status = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		`aria-invalid="true"`,
		"Names can&#39;t have spaces.",
		"The interval needs a unit — like 5m, 90s or 1h.",
		"must be name=spec",
		"The expected status is a whole number, like 200.",
		`value="bad name!"`,
		`exoModalOpen("checks-add")`,
		`autofocus`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("422 re-render missing %q", want)
		}
	}
	// A refusal never flashes: the render consumes the flash cookie (an empty
	// clearing Set-Cookie), so no flash MESSAGE may ride this response.
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("check refusal carried a flash message")
		}
	}
}

// TestCheckFormKindRefusal asserts a kind the daemon cannot probe refuses on
// the kind select's own meta line (checks_write.go's old kind flash).
func TestCheckFormKindRefusal(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{"name": {"bad"}, "kind": {"weird"}, "interval": {"5m"},
			"target": {"laptop=192.168.1.50:22"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("kind refusal status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Kind is tcp, ping, http, dns, tls or metric.") {
		t.Errorf("kind refusal missing the kind-select sentence")
	}
	if _, err := h.store.GetCheck(context.Background(), "bad"); err == nil {
		t.Fatal("a check was created despite an invalid kind")
	}
}

// TestCheckFormDuplicateRenders asserts a duplicate refusal lands on the name
// field's meta line and keeps the valid fields at rest.
func TestCheckFormDuplicateRenders(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	}); err != nil {
		t.Fatalf("seed check: %v", err)
	}

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{"name": {"at-home"}, "kind": {store.CheckTCP}, "interval": {"5m"},
			"target": {"laptop=192.168.1.50:22"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "A check named at-home already exists.") {
		t.Errorf("duplicate 422 missing the name-field sentence")
	}
	// The interval and target fields are valid and must stay at rest.
	if strings.Contains(body, `id="ck-interval" aria-invalid`) {
		t.Error("duplicate 422 flagged the interval field invalid")
	}
}

// TestCheckAddValidatesTargets: a tcp target without its ip:port shape is
// refused before the store sees it (the same parseCheckTargets `check add`
// runs): the rejection re-renders the form at 422 with the target field's
// sentence, and no row lands.
func TestCheckAddValidatesTargets(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{"name": {"bad"}, "kind": {store.CheckTCP}, "interval": {"5m"}, "target": {"laptop"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("target refusal status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "must be name=spec") {
		t.Errorf("422 re-render missing the target-field sentence")
	}
	if _, err := h.store.GetCheck(context.Background(), "bad"); err == nil {
		t.Fatal("a check was created despite an invalid target")
	}
}

// TestCheckAddBadNameRefused: a name the store's ValidName gate refuses is
// operator input, not a store failure: the form re-renders at 422 with the
// name field's human sentence — never the validator's regex — and no row
// lands.
func TestCheckAddBadNameRefused(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{"name": {"bad name!"}, "kind": {store.CheckTCP}, "interval": {"5m"},
			"target": {"laptop=192.168.1.50:22"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("name refusal status = %d, want 422", resp.StatusCode)
	}
	if strings.Contains(body, "must match") {
		t.Error("name refusal leaks the validator regex")
	}
	if !strings.Contains(body, "Names can&#39;t have spaces.") {
		t.Errorf("name refusal missing the human sentence")
	}
	if _, err := h.store.GetCheck(context.Background(), "bad name!"); err == nil {
		t.Fatal("a check was created despite an invalid name")
	}
}

// TestCheckAddDuplicateRefused: re-adding a name that exists re-renders the
// form at 422 with the duplicate sentence on the name field, and the original
// row survives.
func TestCheckAddDuplicateRefused(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	}); err != nil {
		t.Fatalf("seed check: %v", err)
	}

	resp := postForm(t, op, ts.URL+"/checks/add",
		url.Values{"name": {"at-home"}, "kind": {store.CheckTCP}, "interval": {"5m"},
			"target": {"laptop=192.168.1.50:22"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "A check named at-home already exists.") {
		t.Errorf("duplicate 422 missing the duplicate sentence")
	}
	if _, err := h.store.GetCheck(ctx, "at-home"); err != nil {
		t.Fatalf("original check vanished: %v", err)
	}
}

func TestCheckAddReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))

	resp := postForm(t, ro, ts.URL+"/checks/add",
		url.Values{"name": {"at-home"}, "kind": {store.CheckTCP}, "interval": {"5m"},
			"target": {"laptop=192.168.1.50:22"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly check add = %d, want 403", resp.StatusCode)
	}
}

func TestCheckEnableDisable(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	seedTCPCheck(t, h)

	resp := postForm(t, op, ts.URL+"/checks/at-home/disable", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable = %d, want 303", resp.StatusCode)
	}
	c, err := h.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if c.Enabled {
		t.Fatal("check still enabled after disable")
	}

	resp = postForm(t, op, ts.URL+"/checks/at-home/enable", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable = %d, want 303", resp.StatusCode)
	}
	c, err = h.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if !c.Enabled {
		t.Fatal("check still disabled after enable")
	}
}

func TestCheckRm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	seedTCPCheck(t, h)

	resp := postForm(t, op, ts.URL+"/checks/at-home/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.GetCheck(ctx, "at-home"); err == nil {
		t.Fatal("check still present after rm")
	}
}

// TestCheckRmRefusalOnRow: a confirmed-less remove re-renders the checks
// surface at 422 with the refusal on the row — the forms/SPEC.md button-verb
// shape — never a flash, and nothing is removed.
func TestCheckRmRefusalOnRow(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	seedTCPCheck(t, h)

	resp := postForm(t, op, ts.URL+"/checks/at-home/rm", url.Values{}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("rm refusal status = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		`form-status is-error`,
		"Nothing was removed — confirm the removal in the dialog.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rm refusal missing %q", want)
		}
	}
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("rm refusal flashed")
		}
	}
	if _, err := h.store.GetCheck(ctx, "at-home"); err != nil {
		t.Fatal("refused rm removed the check")
	}
}

// TestCheckRmGoneRefuses: removing a check that is already gone keeps the
// appropriate 404 with human copy — never a flash.
func TestCheckRmGoneRefuses(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/nope/rm", url.Values{"confirmed": {"1"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rm gone status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "No check named nope exists to remove.") {
		t.Errorf("rm gone refusal missing the on-surface sentence")
	}
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("rm gone refusal flashed")
		}
	}
}

// TestCheckEditAtRest: each card carries an edit dialog pre-filled from the
// stored check — interval and name=spec target rows — plus the menu verb that
// opens it. Kind is named but not editable.
func TestCheckEditAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedTCPCheck(t, h)
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks = %d, want 200", code)
	}
	for _, want := range []string{
		`id="check-edit-at-home"`,
		`action="/checks/at-home/update"`,
		`id="check-edit-form-at-home" novalidate`,
		`data-modal-open="check-edit-at-home"`,
		`value="5m0s"`,
		`value="laptop=192.168.1.50:22"`,
		`value="nas=192.168.1.60:22"`,
		"Kind is <code>tcp</code> and is not editable.",
		">Save check</button>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks edit dialog missing %q", want)
		}
	}
}

// TestCheckEditRepointsAndKeepsState: the update handler re-points the check in
// place — targets replace the set, interval replaces the cadence, kind is kept
// — and a kept target NAME preserves its check_state row.
func TestCheckEditRepointsAndKeepsState(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()
	c := seedTCPCheck(t, h)

	resp := postForm(t, op, ts.URL+"/checks/at-home/update",
		url.Values{
			"interval": {"30s"},
			"target":   {"laptop=127.0.0.1:2222", "nas=192.168.1.60:22", "spare=192.168.1.70:22"},
		}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("check update = %d, want 303", resp.StatusCode)
	}

	got, err := h.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if got.Kind != store.CheckTCP {
		t.Fatalf("kind = %q, want tcp (kind is not updatable)", got.Kind)
	}
	if got.IntervalS != 30 || len(got.Targets) != 3 {
		t.Fatalf("updated check = %+v, want 30s and 3 targets", got)
	}
	if got.Targets[0].Name != "laptop" || got.Targets[0].Address != "127.0.0.1" || got.Targets[0].Port != 2222 {
		t.Fatalf("laptop target = %+v, want re-pointed to 127.0.0.1:2222", got.Targets[0])
	}

	st, err := h.store.GetCheckState(ctx, c.ID, "laptop")
	if err != nil {
		t.Fatalf("kept target lost its state row: %v", err)
	}
	if st.Status != store.CheckUp {
		t.Fatalf("kept target state = %q, want its observed up", st.Status)
	}
}

// TestCheckEditRefusalRenders: a bad interval and a malformed target refuse
// together at 422, the edit dialog reopens with every field's sentence and the
// typed values, and the stored row is untouched.
func TestCheckEditRefusalRenders(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()
	seedTCPCheck(t, h)

	resp := postForm(t, op, ts.URL+"/checks/at-home/update",
		url.Values{"interval": {"abc"}, "target": {"laptop"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("edit refusal status = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		`aria-invalid="true"`,
		"The interval needs a unit — like 5m, 90s or 1h.",
		"must be name=spec",
		`value="abc"`,
		`value="laptop"`,
		// The re-render ships the reopen script the edit dialog needs.
		`#checks-surface [data-check-edit-form] [aria-invalid="true"]`,
		`ckeDlg.showModal`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit 422 re-render missing %q", want)
		}
	}
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("edit refusal carried a flash message")
		}
	}
	got, err := h.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if got.IntervalS != 300 || len(got.Targets) != 2 {
		t.Fatalf("a refused update changed the row: %+v", got)
	}
}

func TestCheckEditReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedTCPCheck(t, h)
	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))

	resp := postForm(t, ro, ts.URL+"/checks/at-home/update",
		url.Values{"interval": {"30s"}, "target": {"laptop=192.168.1.50:22"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly check update = %d, want 403", resp.StatusCode)
	}
}

func TestCheckEditGoneRefuses(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/checks/nope/update",
		url.Values{"interval": {"30s"}, "target": {"a=127.0.0.1:22"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("edit gone status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "No check named nope exists to update.") {
		t.Errorf("edit gone refusal missing the on-surface sentence")
	}
}

// fragmentGet issues the same GET the checks tab strip's swap JS makes: the
// surface URL with the X-Requested-With: fragment header, so the server
// answers with the surface region only instead of the full page.
func fragmentGet(t *testing.T, client *http.Client, url string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("X-Requested-With", "fragment")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("fragment GET %s: %v", url, err)
	}
	body := readBody(t, resp)
	return resp.StatusCode, resp.Header, body
}

// TestChecksTabFragmentReturnsRegion: the tab strip's swap fetches the same
// /checks?tab= URL the full page uses, and the fragment response is the
// surface region only — the #checks-surface element, never the layout chrome
// or a flash element — so the JS can swap it in without a full load. The
// region carries the same two data-tab links the full page renders, with the
// requested tab's panel active (m79).
func TestChecksTabFragmentReturnsRegion(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedTCPCheck(t, h)
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, tc := range []struct {
		url, panel string
	}{
		{ts.URL + "/checks", "at-home"},
		{ts.URL + "/checks?tab=netgroups", "Address classification"},
	} {
		code, _, body := fragmentGet(t, op, tc.url)
		if code != http.StatusOK {
			t.Fatalf("fragment %s = %d, want 200", tc.url, code)
		}
		if !strings.Contains(body, `id="checks-surface"`) {
			t.Errorf("fragment %s missing the checks-surface region", tc.url)
		}
		if !strings.Contains(body, tc.panel) {
			t.Errorf("fragment %s missing its panel", tc.url)
		}
	}

	// The fragment is the region only: no layout chrome, no flash element.
	_, _, body := fragmentGet(t, op, ts.URL+"/checks?tab=netgroups")
	for _, banned := range []string{`class="appbar"`, `class="sidebar"`, `class="flash"`, `class="powerline"`} {
		if strings.Contains(body, banned) {
			t.Errorf("fragment leaks layout chrome %q", banned)
		}
	}
	for _, want := range []string{`data-tab="probes"`, `data-tab="netgroups"`} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment missing the tab link %q", want)
		}
	}
}

// TestChecksTabFragmentCarriesTheBarAdd: a tab swap replaces only the surface
// fragment, so the app bar's add verb sits outside it and would stay stale
// (Add check on Probes, Add netgroup on Netgroups). The fragment response must
// carry #appbar-add with the requested tab's own verb, so checksTabJS can
// replace it in place. Dropping @appbarAdd from checksFragment reds this.
func TestChecksTabFragmentCarriesTheBarAdd(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedTCPCheck(t, h)
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, tc := range []struct {
		url, wantLabel, wantTarget string
	}{
		{ts.URL + "/checks", "Add check", "checks-add"},
		{ts.URL + "/checks?tab=netgroups", "Add netgroup", "netgroups-add"},
	} {
		code, _, body := fragmentGet(t, op, tc.url)
		if code != http.StatusOK {
			t.Fatalf("fragment %s = %d, want 200", tc.url, code)
		}
		if !strings.Contains(body, `id="appbar-add"`) {
			t.Errorf("fragment %s missing the appbar-add region", tc.url)
		}
		if !strings.Contains(body, tc.wantLabel) {
			t.Errorf("fragment %s missing the label %q", tc.url, tc.wantLabel)
		}
		if !strings.Contains(body, `data-modal-open="`+tc.wantTarget+`"`) {
			t.Errorf("fragment %s missing the target %q", tc.url, tc.wantTarget)
		}
		if strings.Contains(body, `class="appbar"`) {
			t.Errorf("fragment %s leaks the appbar chrome", tc.url)
		}
	}
}

// TestChecksTabJSOffStillFullLoads: the data-tab links are plain full-load
// anchors (the JS-off path — no <noscript> needed), and the page ships the
// swap script that intercepts them with the fragment fetch, the header, the
// region id, and the re-wire hooks the swapped region's one-shot scripts need.
func TestChecksTabJSOffStillFullLoads(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedTCPCheck(t, h)
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks = %d, want 200", code)
	}
	for _, want := range []string{
		`<a class="tab" data-tab="probes" href="/checks" role="tab" aria-selected="true">Probes</a>`,
		`<a class="tab" data-tab="netgroups" href="/checks?tab=netgroups" role="tab" aria-selected="false">Netgroups</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks missing the full-load anchor %q", want)
		}
	}
	for _, want := range []string{
		`X-Requested-With`, `"fragment"`, `checks-surface`,
		`exoWireCheckForm`, `exoWireNetgroupForm`, `exoEnhanceSelects`,
		`history.pushState`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks missing the swap-script %q", want)
		}
	}
}

// TestChecksTabActiveTabMovesWithSwap: the fragment's strip marks the
// requested tab active — aria-selected rides the server-rendered region, so
// the swap carries the active-tab state with it. The JS-off full load of the
// same URL renders the identical strip, so both paths agree.
func TestChecksTabActiveTabMovesWithSwap(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedTCPCheck(t, h)
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, _, frag := fragmentGet(t, op, ts.URL+"/checks?tab=netgroups")
	for _, want := range []string{
		`data-tab="netgroups" href="/checks?tab=netgroups" role="tab" aria-selected="true"`,
		`data-tab="probes" href="/checks" role="tab" aria-selected="false"`,
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("netgroups fragment missing %q", want)
		}
	}
	_, _, frag = fragmentGet(t, op, ts.URL+"/checks")
	for _, want := range []string{
		`data-tab="probes" href="/checks" role="tab" aria-selected="true"`,
		`data-tab="netgroups" href="/checks?tab=netgroups" role="tab" aria-selected="false"`,
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("probes fragment missing %q", want)
		}
	}

	code, page := getBody(t, op, ts.URL+"/checks?tab=netgroups")
	if code != http.StatusOK {
		t.Fatalf("/checks?tab=netgroups = %d, want 200", code)
	}
	if !strings.Contains(page, `data-tab="netgroups" href="/checks?tab=netgroups" role="tab" aria-selected="true"`) {
		t.Error("JS-off netgroups load does not mark the tab active")
	}
}

// TestChecksTabSwapNeverRefiresFlash: a swap is a partial render, so it can
// neither re-fire the flash the full page consumed nor render a pending one —
// and the fragment render still consumes the cookie, so a later full load
// stays quiet. The flash-cookie model survives the swap (m79).
func TestChecksTabSwapNeverRefiresFlash(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedTCPCheck(t, h)
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// A real flash (the redirectFlash shape) renders on the full page and that
	// render consumes it.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/checks", nil)
	req.AddCookie(&http.Cookie{Name: flashCookie, Value: "added at-home"})
	resp, err := op.Do(req)
	if err != nil {
		t.Fatalf("GET /checks with flash cookie: %v", err)
	}
	if !strings.Contains(readBody(t, resp), "added at-home") {
		t.Error("full page did not render the flash")
	}

	// A swap after that shows the region only: no flash element, no message.
	_, _, frag := fragmentGet(t, op, ts.URL+"/checks?tab=netgroups")
	for _, banned := range []string{"added at-home", `class="flash"`} {
		if strings.Contains(frag, banned) {
			t.Errorf("fragment re-fired the consumed flash %q", banned)
		}
	}

	// A fragment request that itself carries a flash cookie consumes it
	// without rendering it — the swap can neither resurrect a flash nor strand
	// one un-rendered.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/checks?tab=netgroups", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("X-Requested-With", "fragment")
	req.AddCookie(&http.Cookie{Name: flashCookie, Value: "added at-home"})
	resp, err = op.Do(req)
	if err != nil {
		t.Fatalf("fragment with flash cookie: %v", err)
	}
	frag = readBody(t, resp)
	if strings.Contains(frag, "added at-home") || strings.Contains(frag, `class="flash"`) {
		t.Error("fragment rendered a pending flash")
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Error("fragment render did not consume the flash cookie")
	}

	// And a later full load shows nothing.
	_, later := getBody(t, op, ts.URL+"/checks?tab=netgroups")
	if strings.Contains(later, "added at-home") {
		t.Error("flash re-fired on the full page after a fragment fetch")
	}
}

// TestChecksNetgroupAndRecency: the netgroup slot (SPEC §5.1) is the row's
// second line — a route glyph (13px) + "on {netgroup}" (a link to /netgroups)
// · age — so the location claim reads down the column. An observed target uses
// the netgroup the sweep STORED on its state row; an UNOBSERVED one falls back
// to a live classification so it still names where it will be probed ("on
// {netgroup} · away — not probed from here"). The probe duration rides the tile
// as dim meta. An http URL target renders no location verdict at all (its
// liveness is not a location).
func TestChecksNetgroupAndRecency(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.1.0/24")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}

	// tcp targets: laptop inside "home", nas outside every group.
	c, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: "192.168.1.50", Port: 22},
			{Name: "nas", Address: "10.0.0.5", Port: 22},
		},
	})
	if err != nil {
		t.Fatalf("seed tcp check: %v", err)
	}
	// An http target: a URL, no address, so no netgroup classification.
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "web-health", Kind: store.CheckHTTP, IntervalS: 60,
		Targets: []store.CheckTarget{{Name: "web", URL: "https://web.lan/health"}},
	}); err != nil {
		t.Fatalf("seed http check: %v", err)
	}

	// A third target the sweep has never seen: the cold-start unobserved tile.
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "cold", Kind: store.CheckPing, IntervalS: 15,
		Targets: []store.CheckTarget{{Name: "tablet", Address: "192.168.1.60"}},
	}); err != nil {
		t.Fatalf("seed cold check: %v", err)
	}

	// Seed states with LastChange comfortably inside one humanAge bucket: the
	// render happens milliseconds after seeding, so a boundary-seeded offset
	// could truncate one step down before it reaches the page.
	now := time.Now().UTC()
	for _, st := range []store.CheckState{
		{CheckID: c.ID, TargetName: "laptop", Status: store.CheckUp,
			Consecutive: 3, Emitted: store.CheckUp,
			FirstSeen: now.Add(-24 * time.Hour), LastChange: now.Add(-(3*time.Hour + 15*time.Minute)),
			Netgroup: "home", LastDurationMS: 21},
		{CheckID: c.ID, TargetName: "nas", Status: store.CheckDown,
			Consecutive: 3, Emitted: store.CheckDown,
			FirstSeen: now.Add(-24 * time.Hour), LastChange: now.Add(-(2*time.Hour + 15*time.Minute)),
			Netgroup: "away", LastDurationMS: 2500},
	} {
		if err := h.store.SetCheckState(ctx, st); err != nil {
			t.Fatalf("SetCheckState %s: %v", st.TargetName, err)
		}
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks = %d, want 200", code)
	}

	// Location is meta, never a chip (F2): the covered target links its group
	// (→ /netgroups), the uncovered one reads the away miss, and the age rides
	// the same slot. The at-a-glance down counter still names the header.
	for _, want := range []string{
		`<a class="mono" href="/netgroups">on home</a>`,
		"on away",
		"· 3h ago", "· 2h ago",
		"took 21ms", "took 2.5s",
		`1 of 2 down`,
		// The where slot always opens with the 13px route glyph (three nodes).
		"M6 8.2v7.6M18 9.2c0 4.8-5.5 4.3-9.3 6.9",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks missing %q", want)
		}
	}
	// Reachability and location are never two equal chips on the same tile: the
	// netgroup is a link inside the where slot, so no home/away chip survives.
	if strings.Contains(page, `>home</span>`) || strings.Contains(page, `>away</span>`) {
		t.Error("a netgroup still renders as a chip on the checks surface (F2)")
	}

	// The http target renders no location verdict at all: cut its card and
	// check for any location prose.
	seg := page[strings.Index(page, "web-health"):]
	if strings.Contains(seg, "on home") || strings.Contains(seg, "on away") {
		t.Error("http target rendered a location verdict")
	}

	// The never-seen tile is a single dim UNOBSERVED chip that explains itself
	// in the same where slot, never a location chip (F3). Checks render by
	// name, so "cold" sits between the others — slice to the next card.
	cold := page[strings.Index(page, "cold"):strings.Index(page, "web-health")]
	for _, want := range []string{
		`<span class="chip dim"><span class="dot"></span>UNOBSERVED</span>`,
		"on home · away — not probed from here",
	} {
		if !strings.Contains(cold, want) {
			t.Errorf("cold-start tile missing %q", want)
		}
	}
	if strings.Contains(cold, "never seen yet") || strings.Contains(cold, "on away") {
		t.Error("cold-start tile reads a stale copy or the away miss")
	}
}

// TestChecksDownAndHomeAttention: a target DOWN while classified into a
// registered netgroup is attention-worthy — the header counts it and the tile
// carries the note — while an ordinary roaming down (down and away) stays
// quiet. Reverting DownAndHome to always false reds this.
func TestChecksDownAndHomeAttention(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.1.0/24")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}
	c, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: "192.168.1.50", Port: 22},
			{Name: "nas", Address: "10.0.0.5", Port: 22},
		},
	})
	if err != nil {
		t.Fatalf("seed check: %v", err)
	}
	now := time.Now().UTC()
	for _, st := range []store.CheckState{
		{CheckID: c.ID, TargetName: "laptop", Status: store.CheckDown,
			Consecutive: 3, Emitted: store.CheckDown, FirstSeen: now, LastChange: now,
			Netgroup: "home", LastDurationMS: 21},
		{CheckID: c.ID, TargetName: "nas", Status: store.CheckDown,
			Consecutive: 3, Emitted: store.CheckDown, FirstSeen: now, LastChange: now,
			Netgroup: "away", LastDurationMS: 30},
	} {
		if err := h.store.SetCheckState(ctx, st); err != nil {
			t.Fatalf("SetCheckState %s: %v", st.TargetName, err)
		}
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks = %d, want 200", code)
	}
	if !strings.Contains(page, "2 of 2 down · 1 at home") {
		t.Errorf("header does not count the down-at-home target:\n%s", page)
	}
	if got := strings.Count(page, "down while home — expected reachable"); got != 1 {
		t.Errorf("down-at-home note rendered %d times, want 1", got)
	}
	// The roaming-down target (down and away) carries no attention note: slice
	// its tile, which follows the laptop tile.
	nas := page[strings.Index(page, ">nas<"):]
	if strings.Contains(nas, "down while home") {
		t.Error("a roaming-down target rendered the at-home note")
	}
}

// TestChecksLinkToEvents: the check name links to the check source's events,
// so an operator reaches what a check raised. The netgroup is prose now (F2),
// not a chip link.
func TestChecksLinkToEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.1.0/24")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	}); err != nil {
		t.Fatalf("seed check: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/checks")
	if code != 200 {
		t.Fatalf("/checks status = %d", code)
	}
	for _, want := range []string{
		`<a class="mono" href="/events?source=check">at-home</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks missing %q", want)
		}
	}
	if strings.Contains(page, `/events?source=check"><span class="chip`) {
		t.Error("a netgroup chip still links to events; location is prose (F2)")
	}
}

// TestCheckAddTargetControl:the check-add modal carries an Add target control
// that clones the last target input, plus the vanilla JS that wires it — the four
// baseline inputs staying the JS-off baseline.
func TestCheckAddTargetControl(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/checks")
	if code != http.StatusOK {
		t.Fatalf("/checks status = %d, want 200", code)
	}
	for _, want := range []string{
		`id="ck-add-target"`,
		`>Add target</button>`,
		`cloneNode`,
		`inputs.length >= 10`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/checks missing the add-target control %q", want)
		}
	}
}

// TestListEmptyStatesNameNextAction: every registered-resource surface names
// its next action when nothing is registered yet, so a cold start reads as a
// next step instead of a dead end (the brief's empty states must not name
// the tool, but the action).
func TestListEmptyStatesNameNextAction(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, tc := range []struct {
		path, want string
	}{
		{"/checks", "No checks registered — add one."},
		{"/netgroups", "No netgroups registered — add one."},
		{"/rules", "No rules registered — rules are written on the CLI."},
		{"/runners", "No runners registered — they are runner definitions only; add one with <code>exocomp runner add</code>."},
		{"/schedules", "No schedules yet. They fire on the cron you give them — add one with <code>exocomp schedule add</code>."},
		{"/sources", "No sources registered — add one."},
		{"/workflows", "No workflows recorded — they appear when you run <code>exocomp run &lt;source&gt; &lt;playbook&gt;</code>."},
		{"/workspaces", "No workspace registered — exocomp ws add on the host defines one."},
	} {
		code, page := getBody(t, op, ts.URL+tc.path)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", tc.path, code)
		}
		if !strings.Contains(page, tc.want) {
			t.Errorf("%s empty state missing %q", tc.path, tc.want)
		}
	}
}

// TestAPIChecksList: a readonly token reads the check.list projection with
// the seeded check and its target, the same bytes `check list --output json`
// emits.
func TestAPIChecksList(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	if _, err := h.store.CreateCheck(context.Background(), store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	}); err != nil {
		t.Fatalf("seed check: %v", err)
	}

	code, _, body := bearerGet(t, ts.URL, "/api/v1/checks", token)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/checks status = %d, want 200 (%s)", code, body)
	}
	if !strings.Contains(body, `"kind":"`+wire.KindCheckList+`"`) {
		t.Fatalf("checks kind missing: %s", body)
	}
	if !strings.Contains(body, `"name":"at-home"`) || !strings.Contains(body, `"address":"192.168.1.50"`) {
		t.Fatalf("check row not projected: %s", body)
	}
}
