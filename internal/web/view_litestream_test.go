package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/litestream"
)

// seedReplicaFrame lays one WAL frame into a litestream file replica at the
// layout internal/litestream/status_test.go pins — generations/{gen}/wal/
// {frame}.wal.lz4 — aged `age` behind now, and returns the mtime it set so a
// test can compute the lag figure at assertion time instead of hardcoding a
// second a loaded host can drift past. The frame number rides the filename,
// which the card's checkpoint fact reads back verbatim.
func seedReplicaFrame(t *testing.T, dir, frame string, age time.Duration) time.Time {
	t.Helper()
	walDir := filepath.Join(dir, "generations", "abc123", "wal")
	if err := os.MkdirAll(walDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(walDir, frame+".wal.lz4")
	if err := os.WriteFile(p, []byte("frame"), 0o644); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	mtime := time.Now().UTC().Add(-age)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return mtime
}

// wantLag is the figure the card renders for a frame seeded at mtime, measured
// at assertion time — the same lagLabel the card uses, so the test and the
// render agree even when a slow host stretches the seconds between seed and
// render.
func wantLag(mtime time.Time) string {
	return lagLabel(time.Since(mtime).Truncate(time.Second))
}

// noSidecar and withSidecar stub the server's sidecar presence probe so a test
// renders every state deterministically, independent of the machine's PATH.
func noSidecar(h *harness) {
	h.server.litestreamPresence = func() (string, string) { return "", "" }
}

func withSidecar(h *harness) {
	h.server.litestreamPresence = func() (string, string) { return "/usr/bin/litestream", "litestream 0.3.13" }
}

func setReplicaTarget(t *testing.T, h *harness, uri string) {
	t.Helper()
	if err := h.store.PutSetting(context.Background(), litestream.SettingReplicaTarget, uri); err != nil {
		t.Fatalf("PutSetting target: %v", err)
	}
}

func setReplicaAck(t *testing.T, h *harness) {
	t.Helper()
	if err := h.store.PutSetting(context.Background(), litestream.SettingReplicaAck, "true"); err != nil {
		t.Fatalf("PutSetting ack: %v", err)
	}
}

// getPage fetches an authenticated page from a fresh harness server and
// returns its body, failing on any non-200.
func getPage(t *testing.T, h *harness, path string) string {
	t.Helper()
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, client, ts.URL+path)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, code)
	}
	return page
}

// TestLitestreamCardNoReplica: with no replica target the leg renders the dim
// NO REPLICA state — a gap, never a fault — naming the CLI verb that fills it
// (SPEC §3.1).
func TestLitestreamCardNoReplica(t *testing.T) {
	h := newHarness(t)
	withSidecar(h)
	page := getPage(t, h, "/system")

	for _, want := range []string{
		`class="leg" id="litestream"`,
		`data-state="dim"`,
		">NO REPLICA</span>",
		"no target · the store lives on this disk only",
		"exocomp litestream target ‹uri›",
		"litestream 0.3.13 present · runs beside the daemon",
		"Backups still recover the system",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing %q", want)
		}
	}
}

// TestLitestreamCardNoSidecar is the NO SIDECAR GATE: a configured target with
// no engine is a fault (SPEC §3.1), not the "no replica" gap it used to
// collapse into. The MUTATION — dropping the state so a set target without the
// binary renders NO REPLICA — must red this test.
func TestLitestreamCardNoSidecar(t *testing.T) {
	h := newHarness(t)
	noSidecar(h)
	setReplicaTarget(t, h, "/mnt/nas/exocomp-replica")
	page := getPage(t, h, "/system")

	for _, want := range []string{
		`class="leg" id="litestream"`,
		`data-state="fail"`,
		">NO SIDECAR</span>",
		"target set · litestream binary not on PATH · nothing replicates",
		"/mnt/nas/exocomp-replica",
		"install litestream on " + hostname() + ", then doctor",
		"A configured leg with no engine is a failure, not a gap.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing %q", want)
		}
	}
	if strings.Contains(page, ">NO REPLICA</span>") {
		t.Error("a set target with no engine must not render NO REPLICA")
	}
}

// TestLitestreamCardReplicating: a fresh local frame with the sidecar present
// renders the ok REPLICATING state — the lag figure, the checkpoint fact read
// from the frame's own filename and mtime, and the no-pause sentence.
func TestLitestreamCardReplicating(t *testing.T) {
	h := newHarness(t)
	withSidecar(h)
	replica := t.TempDir()
	setReplicaTarget(t, h, replica)
	mtime := seedReplicaFrame(t, replica, "00000000000048211", 2*time.Minute+10*time.Second)
	page := getPage(t, h, "/system")

	for _, want := range []string{
		">REPLICATING</span>",
		wantLag(mtime),
		"behind · RPO",
		"frame 48211",
		"litestream 0.3.13 present · runs beside the daemon",
		"exocomp restore --from-litestream",
		"there is no pause here",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing %q", want)
		}
	}
}

// TestLitestreamCardStale: a frame ≥10m old with the sidecar present renders
// the wait STALE state — the ladder meta and the host command the operator
// runs because the daemon cannot tell why the sidecar stopped.
func TestLitestreamCardStale(t *testing.T) {
	h := newHarness(t)
	withSidecar(h)
	replica := t.TempDir()
	setReplicaTarget(t, h, replica)
	mtime := seedReplicaFrame(t, replica, "00000000000047902", 12*time.Minute)
	page := getPage(t, h, "/system")

	for _, want := range []string{
		">STALE</span>",
		">" + wantLag(mtime) + "<",
		"behind · ok &lt; 10m · wait &lt; 1h · fail",
		"present · is it running? exocomp cannot tell",
		"Check the sidecar on " + hostname() + ": systemctl status litestream",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing %q", want)
		}
	}
}

// TestLitestreamCardBehind: a frame ≥1h old is the fail BEHIND state, the
// same card as STALE in the fail colour.
func TestLitestreamCardBehind(t *testing.T) {
	h := newHarness(t)
	withSidecar(h)
	replica := t.TempDir()
	setReplicaTarget(t, h, replica)
	mtime := seedReplicaFrame(t, replica, "00000000000041000", 2*time.Hour)
	page := getPage(t, h, "/system")

	for _, want := range []string{
		">BEHIND</span>",
		">" + wantLag(mtime) + "<",
		"behind · ok &lt; 10m · wait &lt; 1h · fail",
		"systemctl status litestream",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing %q", want)
		}
	}
}

// TestLitestreamCardRemote: a remote replica's frame age only litestream can
// read, so the leg renders the dim REMOTE state with the ack fact the operator
// accepted at persist time made visible (SPEC §3.1).
func TestLitestreamCardRemote(t *testing.T) {
	h := newHarness(t)
	withSidecar(h)
	setReplicaTarget(t, h, "s3://exocomp-replica/nibbler")
	setReplicaAck(t, h)
	page := getPage(t, h, "/system")

	for _, want := range []string{
		">REMOTE</span>",
		">unknown<",
		"lag · only litestream can read a remote replica",
		"none · unencrypted remote acknowledged",
		"litestream 0.3.13 present · runs beside the daemon",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing %q", want)
		}
	}
}

// TestLitestreamCardUnparseable: a stored target that no longer parses renders
// the wait UNPARSEABLE state with the uri as typed and the fix named — never a
// false "no replica".
func TestLitestreamCardUnparseable(t *testing.T) {
	h := newHarness(t)
	withSidecar(h)
	setReplicaTarget(t, h, "gopher://nope")
	page := getPage(t, h, "/system")

	for _, want := range []string{
		">UNPARSEABLE</span>",
		"gopher://nope",
		"fix the target",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing %q", want)
		}
	}
}

// TestLitestreamPageConfigAndRestore is the GATE for the /system/litestream leg
// page (SPEC §3.2): the generated config renders read-only with secrets named,
// never a value, and `restore from replica` deep-links into the DR drawer's
// replica mode (the drawer itself is L-S4's).
func TestLitestreamPageConfigAndRestore(t *testing.T) {
	h := newHarness(t)
	withSidecar(h)
	setReplicaTarget(t, h, "s3://exocomp-replica/nibbler?region=us-east-1")
	setReplicaAck(t, h)
	page := getPage(t, h, "/system/litestream")

	for _, want := range []string{
		`class="leg" id="litestream"`,
		`id="config"`,
		`class="ls-yml"`,
		"type: s3",
		"bucket: exocomp-replica",
		"LITESTREAM_ACCESS_KEY_ID",
		"LITESTREAM_SECRET_ACCESS_KEY",
		"WARNING",
		`href="/system/litestream#config"`,
		`href="/system/backups?from=litestream#restore"`,
		"restore from replica",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system/litestream missing %q", want)
		}
	}
	// A6 (SPEC §7) landed the Set-target form on this page, so it is no longer
	// form-free; what must not exist is a verb that pretends the daemon runs
	// the sidecar (ADR 0012 — the daemon generates config and observes).
	if !strings.Contains(page, `action="/system/litestream/target"`) {
		t.Error("/system/litestream missing the Set-target form")
	}
	if strings.Contains(page, `action="/system/litestream/pause"`) {
		t.Error("a pause verb would pretend the daemon runs litestream")
	}
}

// TestLitestreamCardChevronViews: the card's ▾ holds the two views the design
// gives the leg — config and the DR-drawer deep-link (copy.txt "▾ items").
func TestLitestreamCardChevronViews(t *testing.T) {
	h := newHarness(t)
	page := getPage(t, h, "/system")

	for _, want := range []string{
		`class="ramenu"`,
		`href="/system/litestream#config"`,
		`href="/system/backups?from=litestream#restore"`,
		">config</a>",
		">restore from replica</a>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing %q", want)
		}
	}
}

// TestLitestreamPowerlineNoSidecar: the powerline's litestream segment must
// not stay dim "unknown" while the card reads fail NO SIDECAR — a configured
// target with no engine is loud in both places, and the segment names the leg
// + its word (`litestream no sidecar`), never the retired `rpo` metric.
func TestLitestreamPowerlineNoSidecar(t *testing.T) {
	h := newHarness(t)
	noSidecar(h)
	setReplicaTarget(t, h, "/mnt/nas/exocomp-replica")
	page := getPage(t, h, "/system")

	if !strings.Contains(page, `data-state="fail">litestream no sidecar</a>`) {
		t.Error("powerline litestream segment must read fail 'litestream no sidecar' for a set target with no sidecar")
	}
}

// TestLitestreamPageSetTargetForm is the GATE for A6's form (SPEC §7 A6): the
// /system/litestream page renders the Set-target form — the replica-uri field,
// the unencrypted-remote ack checkbox, the CLI echo — with the honest boundary
// stated: the daemon cannot verify a target, so the form writes the two
// settings only. The stored target and ack render in the controls at rest.
func TestLitestreamPageSetTargetForm(t *testing.T) {
	h := newHarness(t)
	withSidecar(h)
	setReplicaTarget(t, h, "s3://exocomp-replica/nibbler")
	setReplicaAck(t, h)
	page := getPage(t, h, "/system/litestream")

	for _, want := range []string{
		`id="target"`,
		`action="/system/litestream/target"`,
		`name="replica_target"`,
		`value="s3://exocomp-replica/nibbler"`,
		`name="replica_unencrypted_ack" value="1" checked`,
		"I understand this remote is unencrypted",
		"same as exocomp litestream target s3://exocomp-replica/nibbler --ack-unencrypted",
		"the daemon cannot verify",
		"writes the two settings",
		`id="config"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system/litestream missing %q", want)
		}
	}
}

// TestLitestreamSetTargetWritesBothSettings is the GATE for the A6 write (SPEC
// §7 A6): a submit persists the replica target and the ack — the two settings,
// nothing else — and redirects 303 with the success flash. A remote target with
// the ack writes ack=true; a local target writes ack=false (the caveat only
// applies to remote targets). The MUTATION — dropping either PutSetting — must
// red this test.
func TestLitestreamSetTargetWritesBothSettings(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	resp := postForm(t, client, ts.URL+"/system/litestream/target",
		url.Values{"replica_target": {"s3://exocomp-replica/nibbler"}, "replica_unencrypted_ack": {"1"}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("set target = %d, want 303", resp.StatusCode)
	}
	if raw, err := h.store.GetSetting(ctx, litestream.SettingReplicaTarget); err != nil || raw != "s3://exocomp-replica/nibbler" {
		t.Fatalf("replica_target = %q (%v), want the submitted uri", raw, err)
	}
	if ack, err := h.store.GetSetting(ctx, litestream.SettingReplicaAck); err != nil || ack != "true" {
		t.Fatalf("replica_unencrypted_ack = %q (%v), want true", ack, err)
	}

	// Success-only flash: the 303 carries the one-line result, and no refusal
	// can flash (the refusals re-render 422 with the field meta instead).
	code, page := getBody(t, client, ts.URL+"/system/litestream")
	if code != http.StatusOK {
		t.Fatalf("/system/litestream after write = %d, want 200", code)
	}
	if !strings.Contains(page, "replica target set") {
		t.Error("success flash did not render")
	}

	// A local target writes ack=false — the caveat only bites remote.
	resp = postForm(t, client, ts.URL+"/system/litestream/target",
		url.Values{"replica_target": {"/mnt/nas/exocomp-replica"}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("local set target = %d, want 303", resp.StatusCode)
	}
	if raw, _ := h.store.GetSetting(ctx, litestream.SettingReplicaTarget); raw != "/mnt/nas/exocomp-replica" {
		t.Fatalf("replica_target after local write = %q", raw)
	}
	if ack, _ := h.store.GetSetting(ctx, litestream.SettingReplicaAck); ack != "false" {
		t.Fatalf("replica_unencrypted_ack after local write = %q, want false", ack)
	}
}

// TestLitestreamSetTargetRefusesRemoteWithoutAck is the GATE for the ack guard
// (SPEC §7 A6): a remote target without the acknowledgement is refused at 422
// with the human refusal on the ack's meta line, and neither setting is
// written — a refusal re-renders, it never flashes a success. The MUTATION —
// dropping the guard — must red this test.
func TestLitestreamSetTargetRefusesRemoteWithoutAck(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/litestream/target",
		url.Values{"replica_target": {"s3://exocomp-replica/nibbler"}}, nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unacked remote = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		"A remote target is not encrypted by Litestream",
		`name="replica_unencrypted_ack"`,
		`value="s3://exocomp-replica/nibbler"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("refusal page missing %q", want)
		}
	}
	ctx := context.Background()
	if _, err := h.store.GetSetting(ctx, litestream.SettingReplicaTarget); err == nil {
		t.Fatal("an unacked remote target must not be persisted")
	}
	if _, err := h.store.GetSetting(ctx, litestream.SettingReplicaAck); err == nil {
		t.Fatal("an unacked remote target must not write the ack")
	}
}

// TestLitestreamSetTargetRefusesJunkTarget: a target that does not parse is
// refused on the field with the human copy, the typed value survives for the
// operator to correct, and nothing is persisted.
func TestLitestreamSetTargetRefusesJunkTarget(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/litestream/target",
		url.Values{"replica_target": {"gopher://nope"}}, nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("junk target = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		"path, file://, s3:// or sftp://",
		`value="gopher://nope"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("refusal page missing %q", want)
		}
	}
	if _, err := h.store.GetSetting(context.Background(), litestream.SettingReplicaTarget); err == nil {
		t.Fatal("a refused target must not be persisted")
	}
}

// TestLitestreamSetTargetReadonlyRefused pins the write floor: a readonly
// token is refused the write, and nothing is persisted.
func TestLitestreamSetTargetReadonlyRefused(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "ro", "readonly"))

	resp := postForm(t, client, ts.URL+"/system/litestream/target",
		url.Values{"replica_target": {"/mnt/nas/exocomp-replica"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("set target with readonly = %d, want 403", resp.StatusCode)
	}
	if _, err := h.store.GetSetting(context.Background(), litestream.SettingReplicaTarget); err == nil {
		t.Fatal("readonly set-target must not write the target")
	}
}

// TestLitestreamDerivationPure pins the state machine as a pure function, so
// the six-word derivation stays reviewable as data even though the HTTP tests
// drive every state end-to-end.
func TestLitestreamDerivationPure(t *testing.T) {
	cases := []struct {
		name string
		obs  litestreamObserved
		want string
	}{
		{name: "no target", want: "NO REPLICA"},
		{name: "unparseable", obs: litestreamObserved{raw: "gopher://nope", parseErr: errors.New("gopher"), binPath: "/usr/bin/litestream"}, want: "UNPARSEABLE"},
		{name: "no sidecar", obs: litestreamObserved{raw: "/srv/replica"}, want: "NO SIDECAR"},
		{name: "remote", obs: litestreamObserved{raw: "s3://b/p", remote: true, binPath: "/usr/bin/litestream", version: "litestream 0.3.13", ack: true}, want: "REMOTE"},
		{name: "replicating", obs: litestreamObserved{raw: "/srv/replica", binPath: "/usr/bin/litestream", version: "litestream 0.3.13", freshOK: true, age: 2*time.Minute + 10*time.Second}, want: "REPLICATING"},
		{name: "stale", obs: litestreamObserved{raw: "/srv/replica", binPath: "/usr/bin/litestream", version: "litestream 0.3.13", freshOK: true, age: 12 * time.Minute}, want: "STALE"},
		{name: "behind", obs: litestreamObserved{raw: "/srv/replica", binPath: "/usr/bin/litestream", version: "litestream 0.3.13", freshOK: true, age: 2 * time.Hour}, want: "BEHIND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveLitestream(tc.obs).State; got != tc.want {
				t.Errorf("deriveLitestream = %q, want %q", got, tc.want)
			}
		})
	}
}
