package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestJobDetailGrantTiles is the run-grant gate: a SANDBOXED run that was
// widened carries the grant tiles — the declared --egress port set and the
// --ssh-identity files it could read — in the RUN CONFIGURATION grid, so an
// operator can see what a sandboxed run was permitted to touch. The value is a
// PATH, never key material. Dropping either tile must red.
func TestJobDetailGrantTiles(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	granted := seedJobWithRecap(t, h, store.StateSucceeded, 0,
		[]string{"PLAY RECAP", "node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0"},
		nil, store.JobArgs{
			Argv:             []string{"/bin/true", "deploy/site.yml"},
			EgressPorts:      []int{2222, 8443},
			SSHIdentityFiles: []string{"/keys/lima", "/keys/prod"},
		})

	_, page := getBody(t, client, ts.URL+"/jobs/"+itoa64(granted.ID))
	for _, want := range []string{
		`<span>Egress ports</span> <b>2222, 8443</b>`,
		`<span>SSH identity</span> <b>/keys/lima, /keys/prod</b>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("granted job detail missing the grant tile %q", want)
		}
	}
}

// TestJobDetailGrantTilesQuietWhenNone is the quiet-default gate: a SANDBOXED
// run with no declaration carries no grant tile, so the default posture stays
// quiet — a run with nothing to show must not invent an empty grant tile.
func TestJobDetailGrantTilesQuietWhenNone(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	plain := seedJobWithRecap(t, h, store.StateSucceeded, 0,
		[]string{"PLAY RECAP", "node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0"},
		nil, store.JobArgs{Argv: []string{"/bin/true", "deploy/site.yml"}})

	_, page := getBody(t, client, ts.URL+"/jobs/"+itoa64(plain.ID))
	if strings.Contains(page, `<span>Egress ports</span>`) {
		t.Error("a sandboxed run with no egress declaration fabricated an Egress ports tile")
	}
	if strings.Contains(page, `<span>SSH identity</span>`) {
		t.Error("a sandboxed run with no ssh identity declaration fabricated an SSH identity tile")
	}
}

// TestJobDetailGrantTilesSuppressedWhenPrivileged is the posture gate: a
// PRIVILEGED run forfeited the sandbox, so no grant applied — the grant tiles
// stay absent and the Privileged tile names the no-sandbox posture instead.
// Showing an egress or ssh-identity grant on an unsandboxed run would imply
// the run was restricted to it, which is a lie.
func TestJobDetailGrantTilesSuppressedWhenPrivileged(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	priv := seedJobWithRecap(t, h, store.StateSucceeded, 0,
		[]string{"PLAY RECAP", "node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0"},
		nil, store.JobArgs{
			Argv:             []string{"/bin/true", "deploy/site.yml"},
			Privileged:       true,
			EgressPorts:      []int{2222},
			SSHIdentityFiles: []string{"/keys/lima"},
		})

	_, page := getBody(t, client, ts.URL+"/jobs/"+itoa64(priv.ID))
	if !strings.Contains(page, `<span>Privileged</span> <b>unsandboxed</b>`) {
		t.Errorf("privileged job detail missing the Privileged tile:\n%s", page)
	}
	if strings.Contains(page, `<span>Egress ports</span>`) {
		t.Error("a privileged run must not carry an Egress ports grant tile (no sandbox, no grant)")
	}
	if strings.Contains(page, `<span>SSH identity</span>`) {
		t.Error("a privileged run must not carry an SSH identity grant tile (no sandbox, no grant)")
	}
}
