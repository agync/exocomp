package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	// Read-only: the whole leg surface carries no form and no input.
	for _, leak := range []string{"<form", "<input"} {
		if strings.Contains(page, leak) {
			t.Errorf("/system/litestream must be read-only — found %q", leak)
		}
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

// TestLitestreamPowerlineNoSidecar: the powerline's rpo segment must not stay
// dim "unknown" while the card reads fail NO SIDECAR — a configured target
// with no engine is loud in both places.
func TestLitestreamPowerlineNoSidecar(t *testing.T) {
	h := newHarness(t)
	noSidecar(h)
	setReplicaTarget(t, h, "/mnt/nas/exocomp-replica")
	page := getPage(t, h, "/system")

	if !strings.Contains(page, `data-state="fail">rpo —</a>`) {
		t.Error("powerline rpo segment must read fail rpo — for a set target with no sidecar")
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
