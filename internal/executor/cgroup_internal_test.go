package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCaps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		mem, cpu   string
		wantMem    int64
		wantQuota  int64
		wantPeriod int64
		wantErr    bool
		wantEmpty  bool
	}{
		{name: "both", mem: "2G", cpu: "200%", wantMem: 2 << 30, wantQuota: 200000, wantPeriod: 100000},
		{name: "memory only", mem: "512M", wantMem: 512 << 20},
		{name: "cpu only", cpu: "50%", wantQuota: 50000, wantPeriod: 100000},
		{name: "cpu no percent sign", cpu: "150", wantQuota: 150000, wantPeriod: 100000},
		{name: "bytes literal", mem: "1073741824", wantMem: 1 << 30},
		{name: "kilo", mem: "4K", wantMem: 4 << 10},
		{name: "empty", wantEmpty: true},
		{name: "bad mem suffix", mem: "2Q", wantErr: true},
		{name: "bad mem number", mem: "xG", wantErr: true},
		{name: "negative mem", mem: "-1G", wantErr: true},
		{name: "zero mem", mem: "0", wantErr: true},
		{name: "bad cpu", cpu: "abc%", wantErr: true},
		{name: "zero cpu", cpu: "0%", wantErr: true},
		// A digit string large enough that n*mult overflows int64 must error,
		// not wrap into a small or negative byte count that memory.max would
		// then write as a wrong, tiny cap (round-2 MINOR finding).
		{name: "overflow mem", mem: "9223372036854775807T", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseCaps(tc.mem, tc.cpu)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCaps(%q,%q) = %+v, want error", tc.mem, tc.cpu, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCaps(%q,%q) unexpected error: %v", tc.mem, tc.cpu, err)
			}
			if got.MemoryMaxBytes != tc.wantMem {
				t.Errorf("MemoryMaxBytes = %d, want %d", got.MemoryMaxBytes, tc.wantMem)
			}
			if got.CPUMaxQuotaUsec != tc.wantQuota {
				t.Errorf("CPUMaxQuotaUsec = %d, want %d", got.CPUMaxQuotaUsec, tc.wantQuota)
			}
			if got.CPUMaxPeriodUsec != tc.wantPeriod {
				t.Errorf("CPUMaxPeriodUsec = %d, want %d", got.CPUMaxPeriodUsec, tc.wantPeriod)
			}
			if got.Empty() != tc.wantEmpty {
				t.Errorf("Empty() = %v, want %v", got.Empty(), tc.wantEmpty)
			}
		})
	}
}

func TestCapsFileContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		caps    Caps
		wantMem string
		wantCPU string
	}{
		{name: "both", caps: Caps{MemoryMaxBytes: 2 << 30, CPUMaxQuotaUsec: 200000, CPUMaxPeriodUsec: 100000},
			wantMem: "2147483648", wantCPU: "200000 100000"},
		{name: "memory only", caps: Caps{MemoryMaxBytes: 1 << 20}, wantMem: "1048576", wantCPU: ""},
		{name: "cpu default period", caps: Caps{CPUMaxQuotaUsec: 50000}, wantMem: "", wantCPU: "50000 100000"},
		{name: "empty", caps: Caps{}, wantMem: "", wantCPU: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.caps.memoryMaxContent(); got != tc.wantMem {
				t.Errorf("memoryMaxContent() = %q, want %q", got, tc.wantMem)
			}
			if got := tc.caps.cpuMaxContent(); got != tc.wantCPU {
				t.Errorf("cpuMaxContent() = %q, want %q", got, tc.wantCPU)
			}
		})
	}
}

// TestApplyCapsWritesFiles proves the exact bytes land in the cgroup files, and
// in particular that memory.swap.max is forced to 0 whenever memory is capped —
// the one write without which a job over its limit swaps instead of being
// OOM-killed. It writes into a plain directory, so it needs no cgroup mount.
func TestApplyCapsWritesFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The kernel presents these as pre-existing files; a plain dir has none, so
	// seed them the way the cgroup fs would.
	for _, f := range []string{"memory.max", "memory.swap.max", "cpu.max"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	caps := Caps{MemoryMaxBytes: 64 << 20, CPUMaxQuotaUsec: 200000, CPUMaxPeriodUsec: 100000}
	if err := applyCaps(dir, caps); err != nil {
		t.Fatalf("applyCaps: %v", err)
	}

	want := map[string]string{
		"memory.max":      "67108864",
		"memory.swap.max": "0",
		"cpu.max":         "200000 100000",
	}
	for name, exp := range want {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != exp {
			t.Errorf("%s = %q, want %q", name, body, exp)
		}
	}
}

// TestApplyCapsMemoryOnlyStillZeroesSwap guards the swap-max rule specifically:
// a memory-only cap must still pin swap to 0.
func TestApplyCapsMemoryOnlyStillZeroesSwap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, f := range []string{"memory.max", "memory.swap.max"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyCaps(dir, Caps{MemoryMaxBytes: 8 << 20}); err != nil {
		t.Fatalf("applyCaps: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "memory.swap.max"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "0" {
		t.Errorf("memory.swap.max = %q, want 0", body)
	}
}

func TestEnableLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		avail []string
		want  string
	}{
		{avail: []string{"cpu", "io", "memory", "pids"}, want: "+memory +cpu"},
		{avail: []string{"memory"}, want: "+memory"},
		{avail: []string{"cpu"}, want: "+cpu"},
		{avail: []string{"io", "pids"}, want: ""},
	}
	for _, tc := range cases {
		if got := enableLine(tc.avail); got != tc.want {
			t.Errorf("enableLine(%v) = %q, want %q", tc.avail, got, tc.want)
		}
	}
}

func TestReadKeyedUint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.events")
	body := "low 0\nhigh 0\nmax 3\noom 1\noom_kill 2\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readKeyedUint(path, "oom_kill")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("oom_kill = %d, want 2", got)
	}
	if _, err := readKeyedUint(path, "absent"); err == nil {
		t.Error("expected error for missing key")
	}
}

// TestJobCgroupOOMKilled reads the OOM signal out of a faked memory.events.
func TestJobCgroupOOMKilled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.events"),
		[]byte("oom 1\noom_kill 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := &JobCgroup{dir: dir}
	oom, err := j.OOMKilled()
	if err != nil {
		t.Fatal(err)
	}
	if !oom {
		t.Error("OOMKilled() = false, want true")
	}

	// oom_kill 0 must read as not killed.
	if err := os.WriteFile(filepath.Join(dir, "memory.events"),
		[]byte("oom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oom, err = j.OOMKilled()
	if err != nil {
		t.Fatal(err)
	}
	if oom {
		t.Error("OOMKilled() = true, want false")
	}
}

// TestApplyJobCapsFailsClosedWithoutController is the core refusal: a job that
// asked for caps but has no delegated cgroup subtree must be refused, never run
// uncapped. The spec must be left un-placed so a caller that ignored the error
// still could not launch an uncapped-but-marked process.
func TestApplyJobCapsFailsClosedWithoutController(t *testing.T) {
	t.Parallel()
	sb := SandboxOptions{Caps: Caps{MemoryMaxBytes: 1 << 20}} // Cgroup nil
	spec := Spec{}
	td, err := applyJobCaps(sb, &spec)
	if err == nil {
		t.Fatal("applyJobCaps with caps and no controller succeeded; want fail-closed refusal")
	}
	if td != nil {
		t.Error("teardown returned on a refusal; want nil")
	}
	if spec.UseCgroup {
		t.Error("spec marked capped despite refusal")
	}
}

// TestApplyJobCapsEmptyIsNoop proves the default (no caps) path costs nothing
// and never marks the spec.
func TestApplyJobCapsEmptyIsNoop(t *testing.T) {
	t.Parallel()
	spec := Spec{}
	td, err := applyJobCaps(SandboxOptions{}, &spec)
	if err != nil {
		t.Fatalf("applyJobCaps empty: %v", err)
	}
	if spec.UseCgroup {
		t.Error("empty caps placed a cgroup")
	}
	td(SinkFunc(func(Event) {})) // must not panic
}

// fakeCgroupTree builds a directory that mirrors this process's real cgroup path
// under a temp root, so PrepareCgroupController can be exercised without a real
// delegated subtree. It returns the temp root to pass as PrepareCgroupController's
// argument. controllers seeds cgroup.controllers; procs seeds cgroup.procs.
func fakeCgroupTree(t *testing.T, controllers, procs string) (root, base string) {
	t.Helper()
	root = t.TempDir()
	rel, err := os.ReadFile(procSelfCgroup)
	if err != nil {
		t.Skipf("no %s on this host: %v", procSelfCgroup, err)
	}
	var relPath string
	for _, line := range strings.Split(string(rel), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			relPath = p
			break
		}
	}
	if relPath == "" {
		t.Skip("no unified cgroup line for this process")
	}
	base = filepath.Join(root, filepath.Clean("/"+relPath))
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup.controllers"), []byte(controllers), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup.procs"), []byte(procs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, base
}

// TestPrepareControllerVacatesAndEnables proves the setup sequence against a
// faked tree: it vacates the base's processes into the svc leaf, writes the
// controllers into subtree_control, and creates the jobs parent. This is the
// "no internal process" dance the real kernel enforces with EBUSY.
func TestPrepareControllerVacatesAndEnables(t *testing.T) {
	root, base := fakeCgroupTree(t, "cpu io memory pids", "1\n42\n99\n")

	c, err := PrepareCgroupController(root)
	if err != nil {
		t.Fatalf("PrepareCgroupController: %v", err)
	}

	subtree, err := os.ReadFile(filepath.Join(base, "cgroup.subtree_control"))
	if err != nil {
		t.Fatal(err)
	}
	if string(subtree) != "+memory +cpu" {
		t.Errorf("base subtree_control = %q, want %q", subtree, "+memory +cpu")
	}

	// The base's processes were migrated into the svc leaf. Real cgroupfs
	// migrates one process per write to cgroup.procs; a plain temp file cannot
	// accumulate that way (each write truncates), so this asserts the leaf was
	// created and written, not the full pid set — the per-pid migration is what
	// the podman validation exercises.
	svcProcs, err := os.ReadFile(filepath.Join(base, svcLeaf, "cgroup.procs"))
	if err != nil {
		t.Fatalf("svc cgroup.procs: %v", err)
	}
	if len(strings.Fields(string(svcProcs))) == 0 {
		t.Errorf("svc cgroup.procs is empty; vacate migrated nothing")
	}

	// The jobs parent exists and distributes the controllers.
	jobsSubtree, err := os.ReadFile(filepath.Join(c.jobs, "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("jobs subtree_control: %v", err)
	}
	if string(jobsSubtree) != "+memory +cpu" {
		t.Errorf("jobs subtree_control = %q, want %q", jobsSubtree, "+memory +cpu")
	}
}

// TestPrepareControllerMissingControllerFailsClosed proves a host without the
// memory controller delegated is refused, not silently accepted.
func TestPrepareControllerMissingControllerFailsClosed(t *testing.T) {
	root, _ := fakeCgroupTree(t, "io pids", "1\n")
	_, err := PrepareCgroupController(root)
	if err == nil {
		t.Fatal("PrepareCgroupController succeeded without the memory controller; want fail-closed")
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("error %q does not name the missing controller", err)
	}
}

// TestNewJobCgroupWritesCapsAndTearsDown mints a job cgroup in the faked tree
// and checks the cap files, the placement fd, and removal.
func TestNewJobCgroupWritesCapsAndTearsDown(t *testing.T) {
	root, _ := fakeCgroupTree(t, "cpu io memory pids", "1\n")
	c, err := PrepareCgroupController(root)
	if err != nil {
		t.Fatalf("PrepareCgroupController: %v", err)
	}

	jc, err := c.NewJobCgroup(Caps{MemoryMaxBytes: 32 << 20, CPUMaxQuotaUsec: 100000, CPUMaxPeriodUsec: 100000})
	if err != nil {
		t.Fatalf("NewJobCgroup: %v", err)
	}
	if jc.FD() < 0 {
		t.Error("FD() < 0, want a valid descriptor")
	}
	mem, err := os.ReadFile(filepath.Join(jc.Path(), "memory.max"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mem) != "33554432" {
		t.Errorf("memory.max = %q, want 33554432", mem)
	}
	swap, err := os.ReadFile(filepath.Join(jc.Path(), "memory.swap.max"))
	if err != nil {
		t.Fatal(err)
	}
	if string(swap) != "0" {
		t.Errorf("memory.swap.max = %q, want 0", swap)
	}

	// Remove closes the placement fd and rmdir's the cgroup. Real cgroupfs
	// rmdir's a cgroup even with its control files present; a plain temp dir
	// holding regular files cannot, so on this fake tree Remove reports a
	// non-empty error — expected here, and the real rmdir is covered by the
	// podman validation. What must hold on any host is that the fd is released.
	_ = jc.Remove()
	if jc.FD() != -1 {
		t.Errorf("FD() = %d after Remove, want -1 (descriptor released)", jc.FD())
	}
}
