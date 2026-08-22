package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o700) }

func writeExecutable(t *testing.T, path, head string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(head), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		have, floor string
		want        bool
	}{
		{"0.74.0", "0.55.0", true},
		{"0.55.0", "0.55.0", true},
		{"0.54.9", "0.55.0", false},
		{"1.0", "0.55.0", true},
		{"0.55", "0.55.0", true},
		{"0.55.0-rc1", "0.55.0", true},
		{"", "0.55.0", false},
		{"nightly", "0.55.0", false},
	} {
		if got := versionAtLeast(tc.have, tc.floor); got != tc.want {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tc.have, tc.floor, got, tc.want)
		}
	}
}

// TestCoversIsRecursive: every nono path grant covers the whole subtree, so a
// guard that only compared paths for equality would pass a grant one directory
// above the host key.
func TestCoversIsRecursive(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		dir, path string
		want      bool
	}{
		{"/data", "/data/host.key", true},
		{"/data", "/data", true},
		{"/data", "/data/run/job-1", true},
		{"/", "/data/host.key", true},
		{"/data/run", "/data/host.key", false},
		{"/database", "/data/host.key", false},
		{"/data/", "/data/host.key", true},
	} {
		if got := covers(tc.dir, tc.path); got != tc.want {
			t.Errorf("covers(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
		}
	}
}

func TestBackendPort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		url  string
		want int
	}{
		{"http://127.0.0.1:41235/state/edge", 41235},
		{"https://example.invalid/state/edge", HTTPSPort},
		{"http://127.0.0.1/state/edge", 0},
		{"", 0},
		{"::not a url", 0},
	} {
		if got := backendPort(tc.url); got != tc.want {
			t.Errorf("backendPort(%q) = %d, want %d", tc.url, got, tc.want)
		}
	}
}

// TestToolDirsWidensOnlyForScripts: an interpreted tool needs the toolchain
// root beside its bin directory, and a compiled one does not — widening for
// both would grant all of ~/.local for a binary dropped in ~/.local/bin.
func TestToolDirsWidensOnlyForScripts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := mkdirAll(bin); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	script := filepath.Join(bin, "shim")
	writeExecutable(t, script, "#!/usr/bin/env python3\n")
	if got, want := toolDirs(script), []string{bin, dir}; !equalPaths(got, want) {
		t.Errorf("toolDirs(script) = %v, want %v", got, want)
	}

	compiled := filepath.Join(bin, "tool")
	writeExecutable(t, compiled, "\x7fELF\x02\x01\x01\x00")
	if got, want := toolDirs(compiled), []string{bin}; !equalPaths(got, want) {
		t.Errorf("toolDirs(compiled) = %v, want %v", got, want)
	}
}

func equalPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPlanOutPathRedirect covers the plan-artifact rewrite: a tofu `plan -out=`
// aimed at the data dir the sandbox denies is redirected into the granted job
// dir, then the finished plan is relocated back to where the caller will seal
// it from.
func TestPlanOutPathRedirect(t *testing.T) {
	t.Parallel()

	for _, argv := range [][]string{
		{"tofu", "plan", "-out=/data/artifacts/ws.tfplan"},
		{"tofu", "plan", "-out", "/data/artifacts/ws.tfplan"},
	} {
		dst, _ := planOutPath(argv)
		if dst != "/data/artifacts/ws.tfplan" {
			t.Fatalf("planOutPath(%v) = %q", argv, dst)
		}
		rewritten := setPlanOutPath(argv, "/job/p/plan.tfplan")
		if got, _ := planOutPath(rewritten); got != "/job/p/plan.tfplan" {
			t.Errorf("setPlanOutPath(%v) left %q", argv, got)
		}
		if _, orig := planOutPath(argv); planMustNotMutate(orig) != "/data/artifacts/ws.tfplan" {
			t.Errorf("setPlanOutPath mutated the caller's argv: %v", argv)
		}
	}

	if dst, _ := planOutPath([]string{"tofu", "apply", "-auto-approve"}); dst != "" {
		t.Errorf("planOutPath on an apply reported %q, want none", dst)
	}
}

func planMustNotMutate(argv []string) string {
	dst, _ := planOutPath(argv)
	return dst
}

// TestRelocatePlanMovesTheArtifact: the plan the sandboxed subprocess wrote into
// the job dir is moved out before the job dir is torn down, and a run that wrote
// none is not an error.
func TestRelocatePlanMovesTheArtifact(t *testing.T) {
	t.Parallel()

	job := t.TempDir()
	src := filepath.Join(job, "plan.tfplan")
	if err := os.WriteFile(src, []byte("PLAN"), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "artifacts", "ws.tfplan")

	relocatePlan(src, dst, discardSink{})
	body, err := os.ReadFile(dst)
	if err != nil || string(body) != "PLAN" {
		t.Fatalf("relocated plan = %q / %v", body, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source plan still present after relocation")
	}

	// A missing source is a no-op, not a panic or an error.
	relocatePlan(filepath.Join(job, "absent.tfplan"), filepath.Join(t.TempDir(), "x.tfplan"), discardSink{})
}

// TestStagePlanInputCopiesIntoTheJobDir: an `apply <planfile>` naming a plan in
// the data dir the sandbox denies is rewritten to read from the granted job
// dir, with the bytes copied there.
func TestStagePlanInputCopiesIntoTheJobDir(t *testing.T) {
	t.Parallel()

	plan := filepath.Join(t.TempDir(), "saved.tfplan")
	if err := os.WriteFile(plan, []byte("SAVED"), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	jobPrivate := t.TempDir()

	staged, err := stagePlanInput([]string{"tofu", "apply", plan}, jobPrivate)
	if err != nil {
		t.Fatalf("stagePlanInput: %v", err)
	}
	got := staged[2]
	if filepath.Dir(got) != jobPrivate {
		t.Fatalf("apply plan not staged into the job dir: %q", got)
	}
	if body, err := os.ReadFile(got); err != nil || string(body) != "SAVED" {
		t.Fatalf("staged plan = %q / %v", body, err)
	}

	// A plan-less apply is returned untouched.
	argv := []string{"tofu", "apply", "-auto-approve"}
	if out, _ := stagePlanInput(argv, jobPrivate); !equalPaths(out, argv) {
		t.Errorf("stagePlanInput rewrote a plan-less apply: %v", out)
	}
}

type discardSink struct{}

func (discardSink) Emit(Event) {}
