package executor

import (
	"slices"
	"testing"
)

// backendURL stands in for a per-run loopback state backend on an ephemeral
// port, the way api.StateURL builds one.
const egressTestBackend = "http://127.0.0.1:41235/state/edge"

// tofuProfilePorts resolves the ConnectPorts a sandboxed tofu run ends up with
// for the given operator declaration, mirroring sandboxedTofu.Run: an empty
// base grant, the backend and extras folded in, then the kind's HTTPS grant
// added by ProfileFor.
func tofuProfilePorts(t *testing.T, extra []int) []int {
	t.Helper()
	sb := SandboxOptions{DataDir: "/data"}
	sb.ConnectPorts = tofuEgressPorts(sb.ConnectPorts, egressTestBackend, extra)
	return ProfileFor(KindTofu, sb).ConnectPorts
}

// TestTofuEgressDefaultIsBackendAndHTTPS: with nothing declared, a tofu run
// reaches exactly 443 and the state backend — 6443 stays denied. This is the
// invariant the opt-in widening must not erode.
func TestTofuEgressDefaultIsBackendAndHTTPS(t *testing.T) {
	t.Parallel()

	got := tofuProfilePorts(t, nil)
	if want := []int{HTTPSPort, 41235}; !slices.Equal(got, want) {
		t.Fatalf("default tofu egress = %v, want %v", got, want)
	}
	if slices.Contains(got, 6443) {
		t.Fatalf("default tofu egress %v must not include 6443", got)
	}
}

// TestTofuEgressDeclaredPortIsAdditive: a workspace declaring 6443 gets it on
// top of the default grant, while a sibling declaring nothing does not — the
// declaration is per run, not global.
func TestTofuEgressDeclaredPortIsAdditive(t *testing.T) {
	t.Parallel()

	declared := tofuProfilePorts(t, []int{6443})
	if want := []int{HTTPSPort, 6443, 41235}; !slices.Equal(declared, want) {
		t.Fatalf("declared tofu egress = %v, want %v", declared, want)
	}

	sibling := tofuProfilePorts(t, nil)
	if slices.Contains(sibling, 6443) {
		t.Fatalf("sibling with no declaration got 6443: %v", sibling)
	}
}

// TestAnsibleEgressUnchanged: widening tofu must not touch the ansible kind,
// which still reaches only SSH.
func TestAnsibleEgressUnchanged(t *testing.T) {
	t.Parallel()

	got := ProfileFor(KindAnsible, SandboxOptions{DataDir: "/data"}).ConnectPorts
	if want := []int{SSHPort}; !slices.Equal(got, want) {
		t.Fatalf("ansible egress = %v, want %v", got, want)
	}
}
