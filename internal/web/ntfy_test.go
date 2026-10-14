package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// fakeNtfy is an in-memory stand-in for the daemon's ntfy seam: the presence +
// judgment the test sets, and a TestPush that records the call and returns the
// configured error (nil = delivered). refused models the seam's refusal wrap
// (ErrNtfyRefused), which the modal renders with the refusal copy.
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
		Store: h.store, Deriver: h.env, Plans: h.env, Sealer: h.env, Ntfy: f,
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

// ntfyCardHTML returns the ntfy leg section from a rendered /system page so a
// test can assert on the card alone — the page carries three other leg cards
// that also render a leg-facts grid.
func ntfyCardHTML(t *testing.T, page string) string {
	t.Helper()
	start := strings.Index(page, `<section class="leg" id="ntfy"`)
	if start < 0 {
		t.Fatalf("no ntfy section in:\n%s", page)
	}
	end := strings.Index(page[start:], "</section>")
	if end < 0 {
		t.Fatalf("unterminated ntfy section")
	}
	return page[start : start+end]
}

// seedOnChannel writes the ON state: the URL and topic as settings (the model
// now), the token sealed, and the topic label + detail set.
func seedOnChannel(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.PutSetting(ctx, store.SettingNtfyURL, "https://push.lan"); err != nil {
		t.Fatalf("set url setting: %v", err)
	}
	if err := h.store.PutSetting(ctx, store.SettingNtfyTopic, "exocomp-7f3a"); err != nil {
		t.Fatalf("set topic setting: %v", err)
	}
	if err := h.store.PutSetting(ctx, store.SettingNtfyDetail, "full"); err != nil {
		t.Fatalf("set detail: %v", err)
	}
	sealNtfy(t, h, notify.SecretNtfyToken, "tk_abc", "CIPHER-SENTINEL-TOKEN")
}

// onPresence is the seam's judgment for the fully-configured self-hosted channel.
func onPresence() NtfySeamState {
	return NtfySeamState{
		URLSealed: true, TopicSealed: true, TokenSealed: true,
		ServerClass: "self-hosted", Refused: false,
	}
}

// TestNtfyCardOn is the ON tile: the topic label figures with the server class,
// the Edit verb opens the modal, and the facts block names the channel — the
// configured server URL, the sealed token and the push-detail rung with its
// meaning. There is no fan-out mini table (that moved into the modal).
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
	card := ntfyCardHTML(t, page)
	for _, want := range []string{
		`<section class="leg" id="ntfy"`,
		"chip ok",
		">ON</span>",
		"exocomp-7f3a",
		"topic · self-hosted server",
		`data-modal-open="ntfy-modal">Edit</button>`,
		`<dt>server</dt><dd class="ellipsis">https://push.lan</dd>`,
		`<dt>token</dt><dd>sealed</dd>`,
		`<dt>detail</dt><dd>full · names the workspace, host and error, with Approve/Reject</dd>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("ON tile missing %q", want)
		}
	}
	// No fan-out mini table, no "rotate in Secrets" link: those moved in.
	for _, banned := range []string{"leg-mini", "rotate in Secrets", "max on re-notify"} {
		if strings.Contains(card, banned) {
			t.Errorf("ON tile still renders %q", banned)
		}
	}
	// The token never renders: neither the plaintext nor the ciphertext sentinel.
	for _, leak := range []string{"tk_abc", "CIPHER-SENTINEL-"} {
		if strings.Contains(page, leak) {
			t.Errorf("ON surface leaked %q", leak)
		}
	}
}

// TestNtfyCardRefused is the REFUSED tile: a public topic without a token is a
// fail state, and the fix is the modal's token field (Edit, the primary).
func TestNtfyCardRefused(t *testing.T) {
	h := newHarness(t)
	if err := h.store.PutSetting(context.Background(), store.SettingNtfyURL, "https://ntfy.sh"); err != nil {
		t.Fatalf("set url: %v", err)
	}
	if err := h.store.PutSetting(context.Background(), store.SettingNtfyTopic, "exocomp-7f3a"); err != nil {
		t.Fatalf("set topic: %v", err)
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
	card := ntfyCardHTML(t, page)
	for _, want := range []string{
		"chip fail",
		">REFUSED</span>",
		"topic · public server",
		`data-modal-open="ntfy-modal">Edit</button>`,
		"add an access token, or self-host ntfy",
		// The facts block still renders on REFUSED; the token reads missing.
		`<dt>server</dt><dd class="ellipsis">https://ntfy.sh</dd>`,
		`<dt>token</dt><dd class="fail">missing</dd>`,
		`<dt>detail</dt><dd>full · names the workspace, host and error, with Approve/Reject</dd>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("REFUSED tile missing %q", want)
		}
	}
	if !strings.Contains(page, `data-state="fail"`) {
		t.Errorf("REFUSED tile missing the fail ring: %s", page)
	}
	if strings.Contains(card, "max on re-notify") {
		t.Errorf("REFUSED tile rendered the fan-out")
	}
}

// TestNtfyCardOff is the OFF tile: nothing set, the quiet Set up verb, and the
// sentence naming the setup command. The figure is a dim `—`.
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
	card := ntfyCardHTML(t, page)
	for _, want := range []string{
		"chip dim",
		">OFF</span>",
		`<span class="value dim">—</span>`,
		"no channel · nothing set",
		`data-modal-open="ntfy-modal">Set up</button>`,
		"exocomp setup notify",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("OFF tile missing %q", want)
		}
	}
	// OFF names no channel: no facts block, just the sentence.
	if strings.Contains(card, "leg-facts") {
		t.Errorf("OFF tile must carry no facts block; card:\n%s", card)
	}
}

// TestNtfyModalIsTheSurface is the modal contract: the config fields, the detail
// radios, the push preview with BOTH detail samples, the Send test verb that
// posts to the test route, and the status box.
func TestNtfyModalIsTheSurface(t *testing.T) {
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
		`<dialog class="modal" id="ntfy-modal"`,
		`id="ntfy-url"`, `id="ntfy-topic"`, `id="ntfy-token"`,
		`name="detail" value="full"`, `name="detail" value="summary"`, `name="detail" value="terse"`,
		"WHAT A PUSH SAYS",
		`data-detail="full"`, `data-detail="summary"`, `data-detail="terse"`,
		"no action buttons in summary or terse",
		`formaction="/system/ntfy/test"`,
		`class="test-box"`,
		"Plain configuration, not a secret",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("modal missing %q", want)
		}
	}
	// The old drawer's copy is gone: no "values never render", no "rotate".
	for _, banned := range []string{"values never render", "rotate in Secrets", "notify.ntfy_detail"} {
		if strings.Contains(page, banned) {
			t.Errorf("modal still carries stale copy %q", banned)
		}
	}
}

// TestNtfyConfigureSavesSettingsAndDetail is the Save path: url/topic/detail are
// ordinary settings, the token stays sealed, and the redirect lands on /system.
func TestNtfyConfigureSavesSettingsAndDetail(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/configure", url.Values{
		"url":    {"https://push.internal.lan"},
		"topic":  {"homelab-alerts"},
		"detail": {"terse"},
		"token":  {"tk_secret12345"},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("configure = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/system" {
		t.Errorf("configure Location = %q, want /system", loc)
	}
	ctx := context.Background()
	if u, _ := h.store.GetSetting(ctx, store.SettingNtfyURL); u != "https://push.internal.lan" {
		t.Errorf("SettingNtfyURL = %q", u)
	}
	if top, _ := h.store.GetSetting(ctx, store.SettingNtfyTopic); top != "homelab-alerts" {
		t.Errorf("SettingNtfyTopic = %q", top)
	}
	if d, _ := h.store.GetSetting(ctx, store.SettingNtfyDetail); d != "terse" {
		t.Errorf("SettingNtfyDetail = %q, want terse", d)
	}
	if _, err := h.store.GetSecret(ctx, notify.SecretNtfyToken); err != nil {
		t.Errorf("ntfy token not sealed: %v", err)
	}
	// URL and topic are settings, never sealed.
	if _, err := h.store.GetSecret(ctx, notify.SecretNtfyURL); err == nil {
		t.Errorf("SecretNtfyURL was unexpectedly sealed")
	}
	if _, err := h.store.GetSecret(ctx, notify.SecretNtfyTopic); err == nil {
		t.Errorf("SecretNtfyTopic was unexpectedly sealed")
	}
}

// TestNtfyConfigureNormalizesDetail: an unrecognized detail falls back to the
// documented full default.
func TestNtfyConfigureNormalizesDetail(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/configure", url.Values{
		"url": {"https://push.lan"}, "topic": {"t"}, "detail": {"garbage"},
	}, nil)
	_ = resp.Body.Close()
	if d, _ := h.store.GetSetting(context.Background(), store.SettingNtfyDetail); d != "full" {
		t.Fatalf("normalized detail = %q, want full", d)
	}
}

// TestNtfyConfigureClearsLegacySealedURLTopic is the upgrade gate: a legacy
// sealed ntfy/url or ntfy/topic (from before those became settings) is cleared
// on save, so a stale secret can never shadow the setting the operator just
// wrote — the readers are secret-first.
func TestNtfyConfigureClearsLegacySealedURLTopic(t *testing.T) {
	h := newHarness(t)
	sealNtfy(t, h, notify.SecretNtfyURL, "https://old.lan", "CIPHER-URL")
	sealNtfy(t, h, notify.SecretNtfyTopic, "old-topic", "CIPHER-TOPIC")
	srv := serverWithNtfy(t, h, &fakeNtfy{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/configure", url.Values{
		"url": {"https://new.lan"}, "topic": {"new-topic"},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("configure = %d, want 303", resp.StatusCode)
	}
	ctx := context.Background()
	if _, err := h.store.GetSecret(ctx, notify.SecretNtfyURL); err == nil {
		t.Error("legacy sealed ntfy/url was not cleared")
	}
	if _, err := h.store.GetSecret(ctx, notify.SecretNtfyTopic); err == nil {
		t.Error("legacy sealed ntfy/topic was not cleared")
	}
	if u, _ := h.store.GetSetting(ctx, store.SettingNtfyURL); u != "https://new.lan" {
		t.Errorf("SettingNtfyURL = %q, want the saved value", u)
	}
}

// TestNtfyConfigureRejectsUserinfo: a URL carrying credentials is refused — the
// token is where a credential belongs, and the URL is stored as plain settings.
func TestNtfyConfigureRejectsUserinfo(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/configure", url.Values{
		"url": {"https://user:pass@push.lan"}, "topic": {"t"},
	}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("userinfo URL = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Put credentials in the access token") {
		t.Errorf("userinfo refusal missing: %s", body)
	}
}

// TestNtfyTestPostsPush is the Send test ok path: it saves the form, posts one
// push through the seam, and re-renders the modal with the delivered status.
func TestNtfyTestPostsPush(t *testing.T) {
	h := newHarness(t)
	seedOnChannel(t, h)
	fake := &fakeNtfy{st: onPresence()}
	srv := serverWithNtfy(t, h, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/test", url.Values{
		"url": {"https://push.lan"}, "topic": {"exocomp-7f3a"},
	}, nil)
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
		`class="test-box is-ok"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("delivered status missing %q: %s", want, body)
		}
	}
}

// TestNtfyTestRefusalStatus is the refusal path: a channel that cannot load (the
// NewNtfy rule) writes the refusal copy in the status box.
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

	resp := postForm(t, client, ts.URL+"/system/ntfy/test", url.Values{
		"url": {"https://ntfy.sh"}, "topic": {"t"}, "token": {"tk_x"},
	}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test = %d, want 200; body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		`class="test-box is-error"`,
		">the server refused the token</span>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal status missing %q: %s", want, body)
		}
	}
}

// TestNtfyTestTimeoutReason is the delivery-failure copy: a transport timeout
// reads as a plain reason, never the opaque generic line and never the error
// text (which can carry the server host).
func TestNtfyTestTimeoutReason(t *testing.T) {
	h := newHarness(t)
	seedOnChannel(t, h)
	fake := &fakeNtfy{st: onPresence(), derr: &net.DNSError{Err: "i/o timeout", IsTimeout: true}}
	srv := serverWithNtfy(t, h, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/ntfy/test", url.Values{
		"url": {"https://push.lan"}, "topic": {"exocomp-7f3a"},
	}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, ">the push timed out — the server did not answer</span>") {
		t.Errorf("timeout status missing: %s", body)
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

// TestNtfyTestUnavailableWithoutSeam: a surface with no ntfy seam answers 503
// rather than pretending to push.
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

// TestNtfyModalAndConfigure tests the setup-to-edit transition: OFF offers Set
// up, a self-hosted configure flips the verb to Edit and pre-fills the fields,
// and a public URL without a token is refused.
func TestNtfyModalAndConfigure(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `data-modal-open="ntfy-modal">Set up</button>`) {
		t.Errorf("expected Set up button opening ntfy-modal, got: %s", page)
	}
	if !strings.Contains(page, `<dialog class="modal" id="ntfy-modal"`) {
		t.Errorf("expected ntfy-modal dialog, got: %s", page)
	}

	// Public ntfy URL without a token is refused.
	resp := postForm(t, client, ts.URL+"/system/ntfy/configure", url.Values{
		"url": {"https://ntfy.sh"}, "topic": {"my-alerts"},
	}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("public ntfy without token = %d, want 422", resp.StatusCode)
	}
	refusalBody := readBody(t, resp)
	if !strings.Contains(refusalBody, "ntfy.sh requires an access token") {
		t.Errorf("expected public ntfy token refusal, got: %s", refusalBody)
	}

	// Self-hosted without a token succeeds.
	resp = postForm(t, client, ts.URL+"/system/ntfy/configure", url.Values{
		"url": {"https://push.internal.lan"}, "topic": {"homelab-alerts"},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("self-hosted configure = %d, want 303", resp.StatusCode)
	}

	// Now configured: the verb becomes Edit and the modal pre-fills the values.
	code, page = getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system after configure = %d, want 200", code)
	}
	if !strings.Contains(page, `data-modal-open="ntfy-modal">Edit</button>`) {
		t.Errorf("expected Edit button opening ntfy-modal after configuration, got: %s", page)
	}
	if !strings.Contains(page, `value="https://push.internal.lan"`) || !strings.Contains(page, `value="homelab-alerts"`) {
		t.Errorf("expected modal to pre-fill configured url and topic, got: %s", page)
	}
}
