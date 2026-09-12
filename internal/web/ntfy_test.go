package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// fakeNtfy is an in-memory stand-in for the daemon's ntfy seam: the presence +
// judgment the test sets, and a TestPush that records the call and returns the
// configured error (nil = delivered). refused models the seam's refusal wrap
// (ErrNtfyRefused), which the drawer renders with the refusal copy.
type fakeNtfy struct {
	st      NtfySeamState
	derr    error
	refused bool
	push    int
}

func (f *fakeNtfy) State(context.Context) (NtfySeamState, error) {
	return f.st, nil
}

func (f *fakeNtfy) TestPush(context.Context, string) error {
	f.push++
	if f.refused {
		return fmt.Errorf("notify: test push refused: %w", ErrNtfyRefused)
	}
	return f.derr
}

// serverWithNtfy builds a web Server over the harness store with an ntfy seam
// attached, mirroring what `serve --web-addr` wires. A nil seam is a surface
// built without one.
func serverWithNtfy(t *testing.T, h *harness, f NtfyDriver) *Server {
	t.Helper()
	srv, err := New(Options{
		Store: h.store, Deriver: h.env, Plans: h.env, Ntfy: f,
		Version: "test", DataDir: h.dir,
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

// sealNtfy seals one ntfy secret row with a value plus a ciphertext sentinel,
// so a test can prove neither ever reaches the page.
func sealNtfy(t *testing.T, h *harness, name, value, sentinel string) {
	t.Helper()
	sealed, err := h.env.Seal(name, "generic", []byte(value))
	if err != nil {
		t.Fatalf("seal %s: %v", name, err)
	}
	if _, err := h.store.CreateSecret(context.Background(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce, Ciphertext: []byte(sentinel),
		AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

// seedOnChannel seals the three keys and the topic label for the ON state.
func seedOnChannel(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	sealNtfy(t, h, notify.SecretNtfyURL, "https://push.lan", "CIPHER-SENTINEL-URL")
	sealNtfy(t, h, notify.SecretNtfyTopic, "exocomp-7f3a", "CIPHER-SENTINEL-TOPIC")
	sealNtfy(t, h, notify.SecretNtfyToken, "tk_abc", "CIPHER-SENTINEL-TOKEN")
	if err := h.store.PutSetting(ctx, store.SettingNtfyTopic, "exocomp-7f3a"); err != nil {
		t.Fatalf("set topic label: %v", err)
	}
	if err := h.store.PutSetting(ctx, store.SettingNtfyDetail, "descriptive"); err != nil {
		t.Fatalf("set detail: %v", err)
	}
}

// onPresence is the seam's judgment for the fully-sealed self-hosted channel.
func onPresence() NtfySeamState {
	return NtfySeamState{
		URLSealed: true, TopicSealed: true, TokenSealed: true,
		ServerClass: "self-hosted", Refused: false,
	}
}

// TestNtfyCardOn is the ON card (SPEC §2.1): the topic label figures with the
// server class, the sealed-key facts, and the 3-row fan-out mini table — a
// table, not toggles.
func TestNtfyCardOn(t *testing.T) {
	h := newHarness(t)
	seedOnChannel(t, h)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		`<section class="leg" id="ntfy"`,
		">ntfy</span>",
		"chip ok",
		">ON</span>",
		"exocomp-7f3a",
		"topic · self-hosted server",
		"rotate in Secrets",
		"descriptive · pushes name the job and host",
		`class="leg-mini"`,
		"approvals",
		"job failures",
		"dead-man tripped",
		"max on re-notify",
		"one page per job",
		">once</span>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("ON card missing %q", want)
		}
	}
	// The powerline reads on.
	if !strings.Contains(page, ">ntfy on</a>") {
		t.Errorf("powerline missing ntfy on: %s", page)
	}
	// Never a sealed value: neither the plaintext nor the ciphertext sentinels.
	for _, leak := range []string{"https://push.lan", "tk_abc", "CIPHER-SENTINEL-"} {
		if strings.Contains(page, leak) {
			t.Errorf("ON card leaked %q", leak)
		}
	}
}

// TestNtfyCardRefused is the REFUSED card: a public topic without a token is a
// fail state with the Seal token verb, never a quiet off.
func TestNtfyCardRefused(t *testing.T) {
	h := newHarness(t)
	if err := h.store.PutSetting(context.Background(), store.SettingNtfyTopic, "exocomp-7f3a"); err != nil {
		t.Fatalf("set topic label: %v", err)
	}
	srv := serverWithNtfy(t, h, &fakeNtfy{st: NtfySeamState{
		URLSealed: true, TopicSealed: true, TokenSealed: false,
		ServerClass: "public", Refused: true,
	}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		"chip fail",
		">REFUSED</span>",
		"topic · public server",
		"token <span class=\"nope\">missing</span>",
		"a public topic without a token would publish approval links to anyone who guesses the name",
		"approvals and failures are silent",
		">Seal token</a>",
		`href="/secrets?key=ntfy/token"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("REFUSED card missing %q", want)
		}
	}
	if !strings.Contains(page, `data-state="fail"`) {
		t.Errorf("REFUSED card missing the fail ring: %s", page)
	}
	// A refused channel must not render the fan-out as if it delivered.
	if strings.Contains(page, "max on re-notify") {
		t.Errorf("REFUSED card rendered the fan-out table")
	}
}

// TestNtfyCardOff is the OFF card: nothing sealed, quiet Set up, and the
// sentence that names the setup command. The figure is a dim `—` with the
// `no channel · nothing sealed` note — never a loud `no channel` at figure
// size (R13).
func TestNtfyCardOff(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		"chip dim",
		">OFF</span>",
		`<span class="value dim">—</span>`,
		"no channel · nothing sealed",
		">Set up</a>",
		"exocomp setup notify",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("OFF card missing %q", want)
		}
	}
	if strings.Contains(page, ">no channel</span>") {
		t.Error("the OFF figure must be a dim '—', never 'no channel' at figure size")
	}
}

// TestNtfyDrawerPresence is the drawer's presence rows (SPEC §2.2): each
// sealed key renders key · dot string (or the topic label) · SEALED, and the
// sealed value never renders — the round's hard rule (b).
func TestNtfyDrawerPresence(t *testing.T) {
	h := newHarness(t)
	seedOnChannel(t, h)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy = %d, want 200", code)
	}
	for _, want := range []string{
		"SYSTEM · NTFY",
		"Push channel",
		`class="presence"`,
		">ntfy/url</span>",
		">ntfy/topic</span>",
		">ntfy/token</span>",
		"self-hosted",
		"SEALED",
		"values never render",
		"rotate in Secrets",
		`action="/system/ntfy/save"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("drawer missing %q", want)
		}
	}
	// Hard rule (b): the sealed values never reach the page.
	for _, leak := range []string{"https://push.lan", "tk_abc", "CIPHER-SENTINEL-"} {
		if strings.Contains(page, leak) {
			t.Errorf("drawer leaked %q", leak)
		}
	}
}

// TestNtfyDrawerMissingToken is the REFUSED drawer: the missing token row
// reads MISSING in fail, and the topic row shows its label.
func TestNtfyDrawerMissingToken(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: NtfySeamState{
		URLSealed: true, TopicSealed: true, TokenSealed: false,
		ServerClass: "public", Refused: true,
	}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy = %d, want 200", code)
	}
	for _, want := range []string{
		"ntfy/token",
		`class="word missing"`,
		">MISSING</span>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("REFUSED drawer missing %q", want)
		}
	}
}

// TestNtfyDrawerDetailAndSample is the Detail two-position switch and the live
// WHAT A PUSH SAYS sample: the stored position is pressed and its sample shows.
func TestNtfyDrawerDetailAndSample(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.PutSetting(ctx, store.SettingNtfyDetail, "terse"); err != nil {
		t.Fatalf("set detail: %v", err)
	}
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy = %d, want 200", code)
	}
	for _, want := range []string{
		"value=\"terse\" aria-pressed=\"true\"",
		"value=\"descriptive\" aria-pressed=\"false\"",
		"notify.ntfy_detail · terse names nothing — for a phone you do not control",
		"WHAT A PUSH SAYS",
		"exocomp: action required",
		"open exocomp to see what",
		"no action buttons in terse",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("terse drawer missing %q", want)
		}
	}
	// The descriptive sample is hidden for a terse channel (its content stays
	// in the markup — `hidden` is a visual, not a scrub).
	if !strings.Contains(page, `id="ntfy-sample-desc" hidden`) {
		t.Errorf("descriptive sample not hidden for terse")
	}
}

// TestNtfyDrawerWhatPages pins the read-only fan-out table (a table, not
// toggles — no routing knobs exist in code).
func TestNtfyDrawerWhatPages(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy = %d, want 200", code)
	}
	for _, want := range []string{
		"WHAT PAGES",
		"<th scope=\"col\">EVENT</th>",
		"<th scope=\"col\">PRIORITY</th>",
		"<th scope=\"col\">RE-NOTIFY</th>",
		"approval waiting",
		"every 10m · max",
		"job failed · degraded",
		"once per job",
		"dead-man tripped",
		">once</td>",
		"one transport, fixed fan-out · <a href=\"#mutes\">quiet hours are a scoped mute</a>, not a setting here",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("what-pages table missing %q", want)
		}
	}
	// No routing knobs: the rows carry no toggle/checkbox controls.
	if strings.Contains(page, `name="route"`) || strings.Contains(page, `type="checkbox"`) {
		t.Errorf("what-pages table rendered routing knobs")
	}
}

// TestNtfyTestPostsPush is the Test verb's ok path: one push through the seam
// and the delivered status line with the time and topic (copy.txt).
func TestNtfyTestPostsPush(t *testing.T) {
	h := newHarness(t)
	seedOnChannel(t, h)
	fake := &fakeNtfy{st: onPresence()}
	srv := serverWithNtfy(t, h, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/test", url.Values{}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if fake.push != 1 {
		t.Fatalf("seam pushed %d times, want 1", fake.push)
	}
	for _, want := range []string{
		"✓ test delivered · ",
		" · exocomp-7f3a",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("delivered status missing %q: %s", want, body)
		}
	}
}

// TestNtfyTestRefusalStatus is the Test verb's refusal path: a channel that
// cannot load (the NewNtfy rule) writes the refusal copy on the status line.
func TestNtfyTestRefusalStatus(t *testing.T) {
	h := newHarness(t)
	fake := &fakeNtfy{st: NtfySeamState{
		URLSealed: true, TopicSealed: true, TokenSealed: false,
		ServerClass: "public", Refused: true,
	}, refused: true}
	srv := serverWithNtfy(t, h, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/test", url.Values{}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test = %d, want 200; body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"class=\"form-status is-error\"",
		">the server refused the token</span>",
		">Retry</a>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal status missing %q: %s", want, body)
		}
	}
}

// TestNtfyTestReadonlyForbidden: a readonly session may not send a push.
func TestNtfyTestReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	fake := &fakeNtfy{st: onPresence()}
	srv := serverWithNtfy(t, h, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "ro", "readonly"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/test", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly test = %d, want 403", resp.StatusCode)
	}
	if fake.push != 0 {
		t.Fatalf("readonly test pushed %d times, want 0", fake.push)
	}
}

// TestNtfyTestUnavailableWithoutSeam: a surface with no ntfy seam (a surface
// built without one) answers 503 rather than pretending to push.
func TestNtfyTestUnavailableWithoutSeam(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/test", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("test without seam = %d, want 503", resp.StatusCode)
	}
}

// TestNtfySaveDetail persists the Detail switch and normalizes an unknown
// value back to descriptive.
func TestNtfySaveDetail(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/save", url.Values{"detail": {"terse"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d, want 303", resp.StatusCode)
	}
	got, err := h.store.GetSetting(context.Background(), store.SettingNtfyDetail)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "terse" {
		t.Fatalf("saved detail = %q, want terse", got)
	}

	resp = postForm(t, client, ts.URL+"/system/ntfy/save", url.Values{"detail": {"garbage"}}, nil)
	_ = resp.Body.Close()
	got, _ = h.store.GetSetting(context.Background(), store.SettingNtfyDetail)
	if got != "descriptive" {
		t.Fatalf("normalized detail = %q, want descriptive", got)
	}
}

// serverWithNtfyAt builds the same surface pinned to `at`: the injected clock
// (Options.Clock) makes the MUTES section's "in …" column deterministic, so a
// test can assert the exact left instead of a window around the read clock.
func serverWithNtfyAt(t *testing.T, h *harness, f NtfyDriver, at time.Time) *Server {
	t.Helper()
	srv, err := New(Options{
		Store: h.store, Deriver: h.env, Plans: h.env, Ntfy: f,
		Version: "test", DataDir: h.dir,
		Clock: func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

// seedMutes writes one mute per scope kind, all expiring at distinct lefts
// against `at` (global 2h, alert 1h, host 4h), and returns their ids in seed
// order. `at` must be millisecond-aligned — the store persists UnixMilli, so a
// sub-ms CreatedAt would read back one truncated millisecond short and shift
// the rendered left.
func seedMutes(t *testing.T, h *harness, at time.Time) []int64 {
	t.Helper()
	ctx := context.Background()
	at = time.UnixMilli(at.UnixMilli()).UTC()
	scopes := []store.NotifyMute{
		{ScopeKind: store.ScopeGlobal},
		{ScopeKind: store.ScopeAlert, ScopeValue: "disk-pressure"},
		{ScopeKind: store.ScopeHost, ScopeValue: "aurora"},
	}
	left := []time.Duration{2 * time.Hour, time.Hour, 4 * time.Hour}
	var ids []int64
	for i, m := range scopes {
		created, err := h.store.CreateNotifyMute(ctx, store.NotifyMute{
			ScopeKind: m.ScopeKind, ScopeValue: m.ScopeValue,
			Reason: "maintenance window", CreatedAt: at, ExpiresAt: at.Add(left[i]),
			CreatedVia: "test",
		})
		if err != nil {
			t.Fatalf("create %s mute: %v", m.ScopeKind, err)
		}
		ids = append(ids, created.ID)
	}
	return ids
}

// TestNtfyDrawerMutes is the MUTES section (SPEC §7 A8): one row per active
// mute — the scope's source · host · until — each with the clear verb, and
// the quiet-hours meta linking here. A global mute is the safety valve: its
// clear asks first, a scoped mute's clear is one click.
func TestNtfyDrawerMutes(t *testing.T) {
	h := newHarness(t)
	at := time.UnixMilli(h.server.now().UnixMilli()).UTC()
	ids := seedMutes(t, h, at)
	srv := serverWithNtfyAt(t, h, &fakeNtfy{st: onPresence()}, at)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy = %d, want 200", code)
	}
	for _, want := range []string{
		"MUTES",
		`<th scope="col">SOURCE</th>`,
		`<th scope="col">HOST</th>`,
		`<th scope="col">UNTIL</th>`,
		// global mute: the scope word, no host, its until, ask-first clear
		">global</td>",
		">—</td>",
		">in 2h 00m</td>",
		// host mute
		">aurora</td>",
		">in 4h 00m</td>",
		// alert mute
		">disk-pressure</td>",
		">in 1h 00m</td>",
		// the What-pages quiet-hours meta points here
		`<a href="#mutes">quiet hours are a scoped mute</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("MUTES section missing %q", want)
		}
	}
	// Every mute renders a clear verb (a dropped verb goes red here).
	for _, id := range ids {
		if !strings.Contains(page, `action="/system/ntfy/mutes/`+itoa64(id)+`/clear"`) {
			t.Errorf("mute %d has no clear verb", id)
		}
	}
	// The global mute's clear asks first; a scoped mute's clear is one click.
	if !strings.Contains(page, `action="/system/ntfy/mutes/`+itoa64(ids[0])+`/clear" class="inline-form" data-confirm=`) {
		t.Errorf("global mute clear missing the ask-first guard")
	}
	for _, id := range []int64{ids[1], ids[2]} {
		if strings.Contains(page, `action="/system/ntfy/mutes/`+itoa64(id)+`/clear" class="inline-form" data-confirm=`) {
			t.Errorf("scoped mute %d clear carried the ask-first guard", id)
		}
	}
}

// TestNtfyDrawerMutesEmpty is the honest empty line: no active mutes renders
// the empty sentence, never a bare table, and the quiet-hours link still
// points at the section.
func TestNtfyDrawerMutesEmpty(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy = %d, want 200", code)
	}
	if !strings.Contains(page, "no active mutes") {
		t.Errorf("empty MUTES section missing the honest empty line: %s", page)
	}
	if !strings.Contains(page, `<a href="#mutes">quiet hours are a scoped mute</a>`) {
		t.Errorf("quiet-hours link missing on an empty MUTES section")
	}
}

// TestNtfyMuteClear is the clear verb: one POST lifts the mute through the
// SAME store.DeleteNotifyMute the CLI's `mute rm` and the API's DELETE /mutes
// call — a direct store call, not a queued job — redirects back to the
// section, and the re-render no longer lists it (a cleared mute still listed
// goes red here).
func TestNtfyMuteClear(t *testing.T) {
	h := newHarness(t)
	at := time.UnixMilli(h.server.now().UnixMilli()).UTC()
	ids := seedMutes(t, h, at)
	srv := serverWithNtfyAt(t, h, &fakeNtfy{st: onPresence()}, at)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK || !strings.Contains(page, ">aurora</td>") {
		t.Fatalf("precondition: MUTES section missing the host mute")
	}

	resp := postForm(t, client, ts.URL+"/system/ntfy/mutes/"+itoa64(ids[2])+"/clear", url.Values{}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear = %d, want 303; body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/system/ntfy#mutes" {
		t.Fatalf("clear Location = %q, want /system/ntfy#mutes", loc)
	}

	rows, err := h.store.ListNotifyMutes(context.Background(), true)
	if err != nil {
		t.Fatalf("ListNotifyMutes: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("store mutes after clear = %+v, want the one host mute gone", rows)
	}
	for _, r := range rows {
		if r.ID == ids[2] {
			t.Fatal("cleared mute still listed in the store")
		}
	}

	code, page = getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy after clear = %d, want 200", code)
	}
	if strings.Contains(page, ">aurora</td>") {
		t.Errorf("cleared mute still rendered on the drawer")
	}
}

// TestNtfyMuteClearReadonlyForbidden: a readonly session may not clear a
// mute — the writeGuard's 403, and the mute stays.
func TestNtfyMuteClearReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	at := time.UnixMilli(h.server.now().UnixMilli()).UTC()
	ids := seedMutes(t, h, at)
	srv := serverWithNtfyAt(t, h, &fakeNtfy{st: onPresence()}, at)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "ro", "readonly"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/mutes/"+itoa64(ids[0])+"/clear", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly clear = %d, want 403", resp.StatusCode)
	}
	rows, err := h.store.ListNotifyMutes(context.Background(), true)
	if err != nil || len(rows) != 3 {
		t.Fatalf("readonly clear touched the store: %+v (%v)", rows, err)
	}
}

// TestPowerlineNtfyRefused is the A11 gate (SPEC §8 step 6): the token-missing
// public-topic case reads `ntfy refused` in fail on the bottom powerline — not
// off.
func TestPowerlineNtfyRefused(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: NtfySeamState{
		URLSealed: true, TopicSealed: true, TokenSealed: false,
		ServerClass: "public", Refused: true,
	}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `>ntfy refused</a>`) {
		t.Errorf("powerline missing ntfy refused: %s", page)
	}
	if !strings.Contains(page, `class="pl-seg fail"`) {
		t.Errorf("refused segment not fail-coloured: %s", page)
	}
}
