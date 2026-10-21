package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	// The show-json companion rides out of the job dir the same way, under the
	// exact sibling name the driver wires up, so the sealer finds both beside
	// the requested -out path.
	showSrc := filepath.Join(job, "plan.tfplan.show.json")
	if err := os.WriteFile(showSrc, []byte(`{"resource_changes":[]}`), 0o600); err != nil {
		t.Fatalf("write show json: %v", err)
	}
	showDst := filepath.Join(t.TempDir(), "artifacts", "ws.tfplan.show.json")
	relocatePlan(showSrc, showDst, discardSink{})
	body2, err := os.ReadFile(showDst)
	if err != nil || string(body2) != `{"resource_changes":[]}` {
		t.Fatalf("relocated show json = %q / %v", body2, err)
	}
	if _, err := os.Stat(showSrc); !os.IsNotExist(err) {
		t.Errorf("source show json still present after relocation")
	}
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

// TestValidatePlanArtifactPathRefusesEscapes: the relocation target for a
// sandboxed `plan -out=` must stay under the data dir's artifacts tree — the
// only place the daemon has promised to write plan material to. A path
// reaching outside it (the data dir root itself, a sibling directory, or
// another tree entirely) is refused rather than silently honored.
func TestValidatePlanArtifactPathRefusesEscapes(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "artifacts", "tofu"), 0o700); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}

	ok := filepath.Join(data, "artifacts", "tofu", "ws-1.tfplan")
	if err := validatePlanArtifactPath(data, ok); err != nil {
		t.Errorf("validatePlanArtifactPath(%q) = %v, want nil", ok, err)
	}

	for _, dst := range []string{
		filepath.Join(data, "host.key"),
		fmt.Sprintf("%s/artifacts/../host.key", data),
		filepath.Join(filepath.Dir(data), "escaped.tfplan"),
		"/etc/passwd",
	} {
		if err := validatePlanArtifactPath(data, dst); err == nil {
			t.Errorf("validatePlanArtifactPath(%q) = nil, want a refusal", dst)
		}
	}
}

// TestSandboxedTofuRefusesPlanOutEscapingArtifacts pins the wiring: a `plan
// -out=` whose relocation target escapes the artifacts tree fails the run
// before any subprocess is started, not just the standalone helper.
func TestSandboxedTofuRefusesPlanOutEscapingArtifacts(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "artifacts", "tofu"), 0o700); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	escaped := filepath.Join(data, "host.key.tfplan")

	drv, ok := NewSandboxedTofu(TofuOptions{
		PrivateDirRoot: t.TempDir(),
	}, SandboxOptions{DataDir: data}).(*sandboxedTofu)
	if !ok {
		t.Fatal("NewSandboxedTofu did not return the sandboxed tofu driver")
	}

	_, err := drv.Run(context.Background(), Spec{
		Kind: KindTofu,
		Argv: []string{"tofu", "plan", "-out=" + escaped},
	}, discardSink{})
	if err == nil {
		t.Fatal("Run with a plan -out escaping the artifacts tree did not fail")
	}
}

// TestAnsibleSourceDirsGrantsExtraVarsFile: a documented flag form,
// `-e @vars.yaml` (resolved to an absolute path before it reaches the argv,
// as AnsiblePassthrough does), must be read-granted like any other source
// file the run names — the `@` prefix marks a file reference, not an
// unreadable value.
func TestAnsibleSourceDirsGrantsExtraVarsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "vars.yaml")
	if err := os.WriteFile(file, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatalf("write vars file: %v", err)
	}

	for _, argv := range [][]string{
		{AnsibleBinary, "--inventory", "localhost,", "--connection", "local", "-e", "@" + file, "site.yml"},
		{AnsibleBinary, "--inventory", "localhost,", "--extra-vars", "@" + file, "site.yml"},
		{AnsibleBinary, "--inventory", "localhost,", "--extra-vars=@" + file, "site.yml"},
	} {
		dirs := ansibleSourceDirs(argv)
		if !slices.Contains(dirs, dir) {
			t.Errorf("ansibleSourceDirs(%v) = %v, want it to grant %s for -e @file", argv, dirs, dir)
		}
	}

	// A plain key=val extra-var names no file and grants nothing extra.
	inline := []string{AnsibleBinary, "--inventory", "localhost,", "-e", "proof=xyz", "site.yml"}
	if dirs := ansibleSourceDirs(inline); slices.Contains(dirs, dir) {
		t.Errorf("ansibleSourceDirs(%v) = %v, an inline -e must not grant a file", inline, dirs)
	}
}

// TestSandboxRunDoesNotCorruptReadAcrossCalls: a *Sandbox is reused across
// repeated Run calls in the same job (the tofu driver runs init, then the
// operation, through the same instance). Run derives a per-call profile from
// s.profile by a shallow struct copy, so profile.Read shares s.profile.Read's
// backing array; slices.DeleteFunc zeroes that array's now-unused tail slots
// in place. Without cloning first, a call whose spec.Dir happens to already
// carry a Read grant (a tool's own directory reused as the job's working
// directory, which ToolGrants and a caller-supplied Dir can coincide on)
// leaves a corrupted "" entry sitting in s.profile.Read for every call after
// it — this proves a second call sees the same, uncorrupted grant the first
// one did.
func TestSandboxRunDoesNotCorruptReadAcrossCalls(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	work := t.TempDir()
	// The Read grant deliberately equals the working directory Run will add
	// to Write, the exact overlap that exposes the aliasing bug.
	sb := NewSandbox(KindCLI, SandboxOptions{DataDir: data, ReadPaths: []string{work}}, recordingDriver{})

	for i := range 2 {
		spec := Spec{Dir: work, Argv: []string{"true"}}
		if _, err := sb.Run(context.Background(), spec, discardSink{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	for _, r := range sb.Profile().Read {
		if r == "" {
			t.Fatalf("s.profile.Read carries a corrupted empty entry after repeated Run calls: %v",
				sb.Profile().Read)
		}
	}
}

// recordingDriver is a no-op inner Driver: the aliasing bug above lives
// entirely in Sandbox.Run's own profile bookkeeping, so the wrapped driver
// only needs to succeed without inspecting argv.
type recordingDriver struct{}

func (recordingDriver) Run(context.Context, Spec, EventSink) (Result, error) {
	return Result{}, nil
}

// capturingDriver keeps the launcher argv Sandbox.Run rewrote its spec into, so
// a test can read back which grants (--read vs --allow) the sandbox handed the
// working directory.
type capturingDriver struct{ argv []string }

func (d *capturingDriver) Run(_ context.Context, spec Spec, _ EventSink) (Result, error) {
	d.argv = spec.Argv
	return Result{}, nil
}

// grantedAs reports whether argv carries `flag dir` as an adjacent pair — the
// shape SandboxProfile.Command emits each grant in (`--read <dir>`,
// `--allow <dir>`).
func grantedAs(argv []string, flag, dir string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == dir {
			return true
		}
	}
	return false
}

// TestSandboxReadOnlyDirGrant is the security proof that Source.Writable is
// honest: a read-only source's working directory is granted --read, never
// --allow, so a job cannot write back to a tree the operator marked read-only;
// a writable source's directory keeps its --allow (write) grant. Before this,
// Sandbox.Run write-granted spec.Dir unconditionally, so the RO bit was defined
// but never enforced.
func TestSandboxReadOnlyDirGrant(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	work := t.TempDir()

	t.Run("read-only source is read-granted, not write-granted", func(t *testing.T) {
		t.Parallel()
		drv := &capturingDriver{}
		sb := NewSandbox(KindAnsible, SandboxOptions{DataDir: data}, drv)
		if _, err := sb.Run(context.Background(),
			Spec{Dir: work, DirReadOnly: true, Argv: []string{"true"}}, discardSink{}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if grantedAs(drv.argv, "--allow", work) {
			t.Fatalf("a read-only source dir was write-granted (--allow %s): %v", work, drv.argv)
		}
		if !grantedAs(drv.argv, "--read", work) {
			t.Fatalf("a read-only source dir was not read-granted (--read %s): %v", work, drv.argv)
		}
	})

	t.Run("writable source is write-granted", func(t *testing.T) {
		t.Parallel()
		drv := &capturingDriver{}
		sb := NewSandbox(KindAnsible, SandboxOptions{DataDir: data}, drv)
		if _, err := sb.Run(context.Background(),
			Spec{Dir: work, DirReadOnly: false, Argv: []string{"true"}}, discardSink{}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if !grantedAs(drv.argv, "--allow", work) {
			t.Fatalf("a writable source dir was not write-granted (--allow %s): %v", work, drv.argv)
		}
		if grantedAs(drv.argv, "--read", work) {
			t.Fatalf("a writable source dir was also read-granted (--read %s), which nono rejects: %v", work, drv.argv)
		}
	})
}

// TestAnsibleScratchEnvRedirectsOffTheSourceTree proves the sandboxed ansible
// run's temp and ssh ControlPath directory resolve inside the per-job granted
// tree and never under the run's working directory, and that remote temp
// resolves there too when the connection is local. That is
// what makes a read-only source checkout as cwd safe: even if the checkout's
// ansible.cfg sets a relative local_tmp or control_path_dir, these env vars win
// and the writes land somewhere the sandbox granted, not on the read-only tree.
func TestAnsibleScratchEnvRedirectsOffTheSourceTree(t *testing.T) {
	t.Parallel()

	// A stand-in read-only source checkout: nothing the scratch env names may
	// resolve under it.
	sourceDir := "/checkout/read-only-source"
	job := jobDir{
		private: "/jobs/job-1/p",
		work:    "/jobs/job-1/w",
		tmp:     "/jobs/job-1/t",
		home:    "/jobs/job-1/h",
	}
	controlPath := filepath.Join(job.home, ".ansible", "cp")

	env := ansibleScratchEnv(job, controlPath)
	get := func(key string) (string, bool) {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, key+"="); ok {
				return v, true
			}
		}
		return "", false
	}

	// Every path ansible writes temp to has to be granted and writable, so it
	// must live under a job grant, and never under the source checkout.
	granted := []string{job.private, job.work, job.tmp} // jobDir.grants()
	for _, key := range []string{"ANSIBLE_LOCAL_TEMP", "ANSIBLE_SSH_CONTROL_PATH_DIR", "TMPDIR"} {
		val, ok := get(key)
		if !ok {
			t.Errorf("%s not set", key)
			continue
		}
		if strings.HasPrefix(val, sourceDir) {
			t.Errorf("%s=%q resolves under the read-only source %q", key, val, sourceDir)
		}
		under := false
		for _, g := range append(granted, controlPath) {
			if val == g || strings.HasPrefix(val, g+"/") {
				under = true
			}
		}
		if !under {
			t.Errorf("%s=%q is not under a granted writable job dir", key, val)
		}
	}
	// The control path is pinned explicitly (not left to ansible.cfg discovery),
	// and it is the granted directory the caller passed.
	if val, _ := get("ANSIBLE_SSH_CONTROL_PATH_DIR"); val != controlPath {
		t.Errorf("ANSIBLE_SSH_CONTROL_PATH_DIR=%q, want the granted control path %q", val, controlPath)
	}

	// Remote temp is made on the far side of the connection, so it names no
	// controller path. A local connection's shell expands it with the job's
	// HOME, which must land in the granted ~/.ansible.
	remote, ok := get("ANSIBLE_REMOTE_TMP")
	if !ok {
		t.Fatal("ANSIBLE_REMOTE_TMP not set")
	}
	if strings.Contains(remote, "/jobs/") {
		t.Errorf("ANSIBLE_REMOTE_TMP=%q names a controller path an ssh target lacks", remote)
	}
	home, _ := get("HOME")
	local := os.Expand(remote, func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	})
	if ansibleHome := filepath.Dir(controlPath); !strings.HasPrefix(local, ansibleHome+"/") {
		t.Errorf("ANSIBLE_REMOTE_TMP=%q expands locally to %q, not under the granted %q",
			remote, local, ansibleHome)
	}
}

// TestAnsibleSourceDirsIgnoresAdhocHostPattern: an ad-hoc run's sole
// positional (argv[1]) is a host PATTERN — a host, a group, or an expression
// like `fedora:!nas` — never a filesystem path. A path-shaped pattern like
// `/` must not read-grant the root it happens to resemble.
func TestAnsibleSourceDirsIgnoresAdhocHostPattern(t *testing.T) {
	t.Parallel()

	argv := []string{AnsibleAdhocBinary, "/", "--inventory", "localhost,",
		"--connection", "local", "--module-name", "command"}
	dirs := ansibleSourceDirs(argv)
	if len(dirs) != 0 {
		t.Errorf("ansibleSourceDirs(%v) = %v, want no grants for a bare host pattern", argv, dirs)
	}
}

// TestCollectionsReadGrantsSplitsOnPathSeparator pins the Decision 0015 sandbox
// fix: a joined ANSIBLE_COLLECTIONS_PATH value names the per-source collections
// dir and the shared fallback, two real directories, and each must be granted
// read individually. Granting the joined string as one path would name a
// directory that does not exist, denying the read a sandboxed run needs.
func TestCollectionsReadGrantsSplitsOnPathSeparator(t *testing.T) {
	t.Parallel()

	perSource := filepath.Join(t.TempDir(), "cache", "collections", "demo-1a2b3c4d", "deadbeef")
	shared := filepath.Join(t.TempDir(), "cache", "collections")
	joined := perSource + string(os.PathListSeparator) + shared

	grants := collectionsReadGrants(joined)
	if !slices.Contains(grants, perSource) || !slices.Contains(grants, shared) {
		t.Fatalf("collectionsReadGrants(%q) = %v, want each component granted", joined, grants)
	}
	if slices.Contains(grants, joined) {
		t.Fatalf("collectionsReadGrants(%q) = %v, granted the joined path as one entry", joined, grants)
	}
	if len(grants) != 2 {
		t.Errorf("collectionsReadGrants(%q) = %v, want exactly the two components", joined, grants)
	}

	// The sandbox profile an ansible run resolves to read-grants each component.
	sb := SandboxOptions{DataDir: t.TempDir(), ReadPaths: collectionsReadGrants(joined)}
	profile := ProfileFor(KindAnsible, sb)
	for _, want := range []string{perSource, shared} {
		if !slices.Contains(profile.Read, want) {
			t.Errorf("sandbox profile missing the read grant %s: %v", want, profile.Read)
		}
	}
}

// TestPythonInstallGrants: the managed-Python install dir is granted when uv is
// configured with an existing absolute one outside the data dir. Every nono
// grant is recursive, so a value that is, holds or sits inside the data dir
// grants nothing instead of exposing job dirs and keys.
func TestPythonInstallGrants(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "python")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	data := t.TempDir()
	if err := os.Mkdir(filepath.Join(data, "run"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dataLink := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(data, dataLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	cases := []struct {
		name string
		env  string
		want []string
	}{
		{"unset", "", nil},
		{"relative", "python", nil},
		{"missing", filepath.Join(target, "absent"), nil},
		{"a file, not a directory", file, nil},
		{"a directory", target, []string{resolved}},
		{"a symlinked directory grants its target", link, []string{resolved}},
		{"the data dir itself", data, nil},
		{"inside the data dir", filepath.Join(data, "run"), nil},
		{"a parent of the data dir", filepath.Dir(data), nil},
		{"a symlink to the data dir", dataLink, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(PythonInstallEnv, tc.env)
			if got := PythonInstallGrants(data); !slices.Equal(got, tc.want) {
				t.Errorf("PythonInstallGrants() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("an unknown data dir grants nothing", func(t *testing.T) {
		t.Setenv(PythonInstallEnv, target)
		if got := PythonInstallGrants(""); got != nil {
			t.Errorf("PythonInstallGrants(\"\") = %v, want nil", got)
		}
	})
	t.Run("a relative data dir is compared as the absolute path", func(t *testing.T) {
		t.Chdir(filepath.Dir(data))
		t.Setenv(PythonInstallEnv, filepath.Join(data, "run"))
		if got := PythonInstallGrants(filepath.Base(data)); got != nil {
			t.Errorf("PythonInstallGrants(relative) = %v, want nil", got)
		}
	})
}

// fakeProbeLauncher stands in for nono: it records each invocation's argv to
// log, fails when the profile names a domain and failProxy is set, and otherwise
// runs the payload after `--` without a kernel sandbox.
func fakeProbeLauncher(t *testing.T, dir, log string, failProxy bool) string {
	t.Helper()
	fail := ""
	if failProxy {
		fail = "case \"$*\" in *--allow-domain*) echo proxy-refused >&2; exit 4;; esac\n"
	}
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" + fail +
		"while [ \"$#\" -gt 0 ] && [ \"$1\" != \"--\" ]; do shift; done\n[ \"$#\" -gt 0 ] && shift\nexec \"$@\"\n"
	path := filepath.Join(dir, "nono")
	writeExecutable(t, path, script)
	return path
}

// probeRun is what one probe assertion against the fakes returned, with the
// launcher invocations it made.
type probeRun struct {
	report string
	proxy  error
	err    error
	calls  []string
}

// probeWithFakes runs the probe's assertion against a fake launcher and a canary
// stub that reports a refusal.
func probeWithFakes(t *testing.T, failProxy bool) probeRun {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	launcher := fakeProbeLauncher(t, dir, log, failProxy)
	canary := filepath.Join(dir, "canary")
	writeExecutable(t, canary, "#!/bin/sh\necho "+canaryPrefix+" file=denied\n")

	var r probeRun
	r.report, r.proxy, r.err = runCanaryProbe(context.Background(), ProbeOptions{Binary: launcher, Canary: canary})
	body, _ := os.ReadFile(log)
	r.calls = strings.Split(strings.TrimSpace(string(body)), "\n")
	return r
}

// TestProbeRunsTheCanaryInBothEgressModes: the resolvers run in proxy mode, so
// the probe asserts the denials there as well as in the port mode.
func TestProbeRunsTheCanaryInBothEgressModes(t *testing.T) {
	r := probeWithFakes(t, false)
	if r.err != nil || r.proxy != nil {
		t.Fatalf("probe = %v, proxy = %v", r.err, r.proxy)
	}
	if r.report == "" || len(r.calls) != 2 {
		t.Fatalf("report %q after %d launcher calls, want one report after two: %v", r.report, len(r.calls), r.calls)
	}
	if !strings.Contains(r.calls[0], "--block-net") || strings.Contains(r.calls[0], "--allow-domain") {
		t.Errorf("port-mode pass = %q, want --block-net and no domain", r.calls[0])
	}
	if strings.Contains(r.calls[1], "--block-net") || !strings.Contains(r.calls[1], "--allow-domain example.invalid") {
		t.Errorf("proxy-mode pass = %q, want --allow-domain and no --block-net", r.calls[1])
	}
}

// TestProxyPassFailureDoesNotDegradeThePortMode: a launcher whose proxy mode
// fails is reported apart, so the other job kinds stay sandboxed and only the
// resolvers refuse to run.
func TestProxyPassFailureDoesNotDegradeThePortMode(t *testing.T) {
	r := probeWithFakes(t, true)
	if r.err != nil {
		t.Fatalf("a failed proxy pass failed the port mode: %v", r.err)
	}
	if r.report == "" {
		t.Error("the port mode reported nothing")
	}
	if r.proxy == nil || !strings.Contains(r.proxy.Error(), "proxy mode") {
		t.Errorf("proxy result = %v, want a proxy mode failure", r.proxy)
	}
}
