package executor

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Per-job resource caps are enforced by writing a cgroup v2 subtree directly,
// not by systemd-run: the shipped runtime is a fedora-minimal container with no
// systemd binary and no user manager, so there is nothing to hand a transient
// scope to (docs/design/byo-runner.md §6). The daemon owns a delegated cgroup
// subtree and creates one child cgroup per opted-in job.
//
// The whole feature is opt-in and fail-closed. Default deployment is rootless
// and capless: no controller is prepared, jobs run exactly as before. When caps
// are requested the controller MUST prepare successfully — a writable, delegated
// v2 subtree with the memory and cpu controllers — or the job is refused. A
// requested cap that cannot be enforced never silently degrades to an uncapped
// run (the same "never silently" posture ADR 0005 holds for the sandbox tier).

// cgroupRoot is where a cgroup v2 hierarchy is mounted in the container. Under
// the quadlet's cgroup delegation this is the container's own namespaced root.
const cgroupRoot = "/sys/fs/cgroup"

// procSelfCgroup names this process's cgroup. On a v2 (unified) host it is one
// line, "0::<path>", and that path is relative to cgroupRoot.
const procSelfCgroup = "/proc/self/cgroup"

// svcLeaf holds the daemon's own processes after the base cgroup is vacated.
// cgroup v2's "no internal processes" rule forbids a cgroup from both holding
// processes and distributing controllers to children, so the daemon has to move
// out of the base cgroup before controllers can be enabled in its subtree — the
// base then becomes a pure distributor with svc and the job cgroups beneath it.
const svcLeaf = "exocomp.svc"

// jobsParent groups every per-job cgroup under one child of the base, kept
// separate from svcLeaf so a job cgroup is never a sibling of the daemon's own
// processes and teardown never races the daemon.
const jobsParent = "exocomp.jobs"

// wantControllers are the controllers a capped job needs. cpu enforces cpu.max;
// memory enforces memory.max and memory.swap.max.
var wantControllers = []string{"memory", "cpu"}

// Caps is one job's resource ceiling, already reduced to the exact bytes and
// microseconds the cgroup files take. Zero fields mean "do not write that file":
// a Caps with only MemoryMaxBytes set caps memory and leaves cpu unbounded.
type Caps struct {
	// MemoryMaxBytes is memory.max. Zero leaves memory uncapped.
	MemoryMaxBytes int64
	// CPUMaxQuotaUsec and CPUMaxPeriodUsec are the two fields of cpu.max: how
	// many microseconds of CPU time the job may use per period. Zero quota
	// leaves cpu uncapped (cpu.max stays "max").
	CPUMaxQuotaUsec  int64
	CPUMaxPeriodUsec int64
}

// Empty reports whether the caps ask for nothing — no file would be written, so
// a cgroup is pointless. The driver treats empty caps as "no caps requested".
func (c Caps) Empty() bool {
	return c.MemoryMaxBytes <= 0 && c.CPUMaxQuotaUsec <= 0
}

// memoryMaxContent is the byte string written to memory.max, or "" when memory
// is uncapped. Kept a pure function so the file assembly is unit-testable
// without a cgroup mount.
func (c Caps) memoryMaxContent() string {
	if c.MemoryMaxBytes <= 0 {
		return ""
	}
	return strconv.FormatInt(c.MemoryMaxBytes, 10)
}

// cpuMaxContent is the byte string written to cpu.max ("<quota> <period>"), or
// "" when cpu is uncapped. A zero or negative period falls back to the kernel
// default of 100000us so a caller that sets only a quota still gets a valid
// pair.
func (c Caps) cpuMaxContent() string {
	if c.CPUMaxQuotaUsec <= 0 {
		return ""
	}
	period := c.CPUMaxPeriodUsec
	if period <= 0 {
		period = defaultCPUPeriodUsec
	}
	return strconv.FormatInt(c.CPUMaxQuotaUsec, 10) + " " + strconv.FormatInt(period, 10)
}

// defaultCPUPeriodUsec is the kernel's default cpu.max period.
const defaultCPUPeriodUsec = 100000

// ParseCaps converts the runner's stored caps — memory in systemd-style size
// syntax ("2G") and cpu as a percentage of one core ("200%") — into the exact
// cgroup file values. Both inputs are optional; an empty string leaves that
// dimension uncapped. It is the one place the operator-facing syntax is
// interpreted, so it is unit-tested against the shapes the store admits.
func ParseCaps(memoryMax, cpuQuota string) (Caps, error) {
	var c Caps
	if s := strings.TrimSpace(memoryMax); s != "" {
		n, err := parseMemorySize(s)
		if err != nil {
			return Caps{}, fmt.Errorf("executor: caps: memory_max %q: %w", memoryMax, err)
		}
		c.MemoryMaxBytes = n
	}
	if s := strings.TrimSpace(cpuQuota); s != "" {
		pct, err := parseCPUPercent(s)
		if err != nil {
			return Caps{}, fmt.Errorf("executor: caps: cpu_quota %q: %w", cpuQuota, err)
		}
		// "200%" of one core over a 100000us period is 200000us of quota.
		c.CPUMaxPeriodUsec = defaultCPUPeriodUsec
		c.CPUMaxQuotaUsec = pct * defaultCPUPeriodUsec / 100
	}
	return c, nil
}

// parseMemorySize reads "2G", "512M", "1073741824" into bytes. The suffixes are
// powers of 1024, matching systemd's MemoryMax and the runner store's intent.
func parseMemorySize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}
	mult := int64(1)
	switch last := s[len(s)-1]; last {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	default:
		if last < '0' || last > '9' {
			return 0, fmt.Errorf("unknown size suffix %q", string(last))
		}
	}
	digits := s
	if mult != 1 {
		digits = strings.TrimSpace(s[:len(s)-1])
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, errors.New("must be positive")
	}
	return n * mult, nil
}

// parseCPUPercent reads "200%" or "200" into the integer percent of one core.
func parseCPUPercent(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, errors.New("must be positive")
	}
	return n, nil
}

// CapsProbe is what the doctor reports for the per-job caps feature: whether a
// capped job could be enforced on this host, and why not when it could not. It
// never reorganizes the cgroup tree — a health read must not vacate the daemon —
// so it tests writability with a throwaway directory and removes it.
type CapsProbe struct {
	// Available is true when a delegated, writable v2 subtree with the memory
	// and cpu controllers is present, so a capped job would be enforced.
	Available bool
	// Base is the daemon's own cgroup, empty when there is no v2 cgroup.
	Base string
	// Controllers are the controllers delegated to Base.
	Controllers []string
	// Detail explains an unavailable result in one line.
	Detail string
}

// ProbeCaps reports whether per-job caps can be enforced here without changing
// anything. root overrides the mount for tests; empty uses cgroupRoot.
func ProbeCaps(root string) CapsProbe {
	if root == "" {
		root = cgroupRoot
	}
	base, err := selfCgroupDir(root)
	if err != nil {
		return CapsProbe{Detail: "no cgroup v2 subtree (" + err.Error() + ")"}
	}
	res := CapsProbe{Base: base}

	avail, err := readControllers(filepath.Join(base, "cgroup.controllers"))
	if err != nil {
		res.Detail = "cannot read cgroup.controllers (" + err.Error() + ")"
		return res
	}
	res.Controllers = avail
	var missing []string
	for _, want := range wantControllers {
		if !contains(avail, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		res.Detail = fmt.Sprintf("controllers not delegated: %v (have %v)", missing, avail)
		return res
	}

	probe := filepath.Join(base, ".exocomp-caps-probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		res.Detail = "cgroup subtree is not writable (" + err.Error() + "); needs cgroup delegation"
		return res
	}
	_ = os.Remove(probe)

	res.Available = true
	res.Detail = "delegated and writable"
	return res
}

// CgroupController owns the daemon's delegated cgroup subtree and mints one
// child cgroup per capped job. It is created once at startup by
// PrepareCgroupController; a nil controller means the caps feature is off.
type CgroupController struct {
	// base is the absolute path of the daemon's own (delegated) cgroup, the one
	// vacated so its subtree can distribute controllers.
	base string
	// jobs is base/jobsParent, the immediate parent of every job cgroup.
	jobs string
}

// PrepareCgroupController readies the delegated subtree for per-job caps, or
// returns an error explaining why caps cannot be enforced here — the caller
// then refuses a capped run rather than running it uncapped (fail-closed). It is
// idempotent: a second call on an already-prepared subtree re-uses it.
//
// root overrides the cgroup mount for tests; empty uses the real cgroupRoot.
func PrepareCgroupController(root string) (*CgroupController, error) {
	if root == "" {
		root = cgroupRoot
	}
	base, err := selfCgroupDir(root)
	if err != nil {
		return nil, err
	}

	avail, err := readControllers(filepath.Join(base, "cgroup.controllers"))
	if err != nil {
		return nil, fmt.Errorf("executor: caps: reading cgroup.controllers under %s: %w; "+
			"the container needs a delegated cgroup v2 subtree "+
			"(see packaging/quadlet/exocomp.container)", base, err)
	}
	for _, want := range wantControllers {
		if !contains(avail, want) {
			return nil, fmt.Errorf("executor: caps: the %q controller is not delegated to %s "+
				"(have %v); enable cgroup delegation for the container", want, base, avail)
		}
	}

	c := &CgroupController{base: base, jobs: filepath.Join(base, jobsParent)}
	if err := c.prepareSubtree(avail); err != nil {
		return nil, err
	}
	return c, nil
}

// prepareSubtree vacates the base cgroup and turns on the controllers its
// children need, then makes the jobs parent a distributor too. Every write is
// checked: a silent failure here would surface later as a job that ran without
// the cap it asked for.
func (c *CgroupController) prepareSubtree(avail []string) error {
	enable := enableLine(avail)

	// The base must hold no processes before its subtree_control can name a
	// controller. Move whatever is there — the daemon and its children — into
	// the svc leaf, then the base is a pure distributor.
	if err := vacate(c.base, filepath.Join(c.base, svcLeaf)); err != nil {
		return err
	}
	if err := writeCgroupFile(filepath.Join(c.base, "cgroup.subtree_control"), enable); err != nil {
		return fmt.Errorf("executor: caps: enabling controllers on %s: %w; "+
			"the delegated subtree may be read-only (needs a writable cgroup mount)", c.base, err)
	}

	if err := os.Mkdir(c.jobs, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("executor: caps: creating jobs cgroup %s: %w", c.jobs, err)
	}
	// The jobs parent distributes the same controllers down to each job leaf.
	if err := writeCgroupFile(filepath.Join(c.jobs, "cgroup.subtree_control"), enable); err != nil {
		return fmt.Errorf("executor: caps: enabling controllers on %s: %w", c.jobs, err)
	}
	return nil
}

// NewJobCgroup mints a child cgroup, writes the cap files into it, and returns a
// handle whose file descriptor places a process into it at fork (clone3
// CLONE_INTO_CGROUP), so the process is never briefly a member of an uncapped
// cgroup. The cap files are written before any process joins: the ceiling is in
// force from the job's first byte.
func (c *CgroupController) NewJobCgroup(caps Caps) (*JobCgroup, error) {
	if c == nil {
		return nil, errors.New("executor: caps: nil controller")
	}
	dir, err := os.MkdirTemp(c.jobs, "job-")
	if err != nil {
		return nil, fmt.Errorf("executor: caps: creating job cgroup: %w", err)
	}

	if err := applyCaps(dir, caps); err != nil {
		_ = os.Remove(dir)
		return nil, err
	}

	// The descriptor must be a directory fd the kernel can clone into. It stays
	// open until the process has been started; Close in teardown releases it.
	fd, err := os.Open(dir)
	if err != nil {
		_ = os.Remove(dir)
		return nil, fmt.Errorf("executor: caps: opening job cgroup %s: %w", dir, err)
	}
	return &JobCgroup{dir: dir, fd: fd}, nil
}

// applyCaps writes the cap files. memory.swap.max is forced to 0 whenever memory
// is capped: without it the kernel lets a job over its memory.max spill into
// swap instead of being OOM-killed, so the ceiling would throttle rather than
// enforce.
func applyCaps(dir string, caps Caps) error {
	if v := caps.memoryMaxContent(); v != "" {
		if err := writeCgroupFile(filepath.Join(dir, "memory.max"), v); err != nil {
			return fmt.Errorf("executor: caps: memory.max: %w", err)
		}
		if err := writeCgroupFile(filepath.Join(dir, "memory.swap.max"), "0"); err != nil {
			return fmt.Errorf("executor: caps: memory.swap.max: %w", err)
		}
	}
	if v := caps.cpuMaxContent(); v != "" {
		if err := writeCgroupFile(filepath.Join(dir, "cpu.max"), v); err != nil {
			return fmt.Errorf("executor: caps: cpu.max: %w", err)
		}
	}
	return nil
}

// JobCgroup is one job's cgroup, alive for the length of the job. FD feeds the
// launcher's CLONE_INTO_CGROUP; Remove tears it down.
type JobCgroup struct {
	dir string
	fd  *os.File
}

// FD is the directory descriptor to place the job process into, or -1 when the
// handle is nil (the uncapped path).
func (j *JobCgroup) FD() int {
	if j == nil || j.fd == nil {
		return -1
	}
	return int(j.fd.Fd())
}

// Path is the job cgroup's directory, for tests and diagnostics.
func (j *JobCgroup) Path() string {
	if j == nil {
		return ""
	}
	return j.dir
}

// OOMKilled reports whether the kernel OOM-killed anything in this cgroup, read
// from memory.events (oom_kill > 0). It is telemetry for the job event, never
// the arbiter of success — the exit code (137 for a SIGKILL) is that, per
// invariant 3.
func (j *JobCgroup) OOMKilled() (bool, error) {
	if j == nil {
		return false, nil
	}
	n, err := readKeyedUint(filepath.Join(j.dir, "memory.events"), "oom_kill")
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Remove kills anything still running in the cgroup and deletes it. It is called
// when the job ends; a grandchild that outlived the job (an ssh control master)
// is killed via cgroup.kill so the rmdir can succeed. Best-effort: a cgroup that
// will not delete is reported to the sink by the caller, not fatal to the job.
func (j *JobCgroup) Remove() error {
	if j == nil {
		return nil
	}
	if j.fd != nil {
		_ = j.fd.Close()
		j.fd = nil
	}
	// cgroup.kill (kernel 5.14+) sends SIGKILL to every process in the subtree
	// at once, which is what lets the empty cgroup be removed.
	_ = writeCgroupFile(filepath.Join(j.dir, "cgroup.kill"), "1")
	if err := os.Remove(j.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("executor: caps: removing job cgroup %s: %w", j.dir, err)
	}
	return nil
}

// selfCgroupDir resolves the absolute path of this process's cgroup under root.
// A v2 host reports one "0::<path>" line; anything else (a v1/hybrid host, or a
// non-Linux one with no such file) is not a delegated v2 subtree this can use.
func selfCgroupDir(root string) (string, error) {
	f, err := os.Open(procSelfCgroup)
	if err != nil {
		return "", fmt.Errorf("executor: caps: %s: %w; per-job caps need a cgroup v2 host", procSelfCgroup, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		rel, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		return filepath.Join(root, filepath.Clean("/"+rel)), nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("executor: caps: reading %s: %w", procSelfCgroup, err)
	}
	return "", errors.New("executor: caps: no unified (v2) cgroup for this process; " +
		"per-job caps need a cgroup v2 host")
}

// vacate moves every process in src into leaf so src can distribute controllers.
// It creates leaf first, then migrates each pid; a pid that has already exited
// is skipped. When src is already empty (a second call) it is a fast no-op.
func vacate(src, leaf string) error {
	pids, err := readCgroupProcs(filepath.Join(src, "cgroup.procs"))
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}
	if err := os.Mkdir(leaf, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("executor: caps: creating svc cgroup %s: %w", leaf, err)
	}
	dst := filepath.Join(leaf, "cgroup.procs")
	for _, pid := range pids {
		// A process that exited between the read and the write is fine; only a
		// real inability to migrate is fatal, because leaving a process in the
		// base means subtree_control will reject the controllers.
		if err := writeCgroupFile(dst, pid); err != nil && !errors.Is(err, os.ErrNotExist) {
			// ESRCH surfaces as a write error on a vanished pid; re-check the
			// source is actually empty before giving up.
			if remaining, rerr := readCgroupProcs(filepath.Join(src, "cgroup.procs")); rerr == nil && len(remaining) == 0 {
				return nil
			}
		}
	}
	return nil
}

// enableLine builds the "+memory +cpu" string for cgroup.subtree_control from
// the controllers actually available, so a host missing one does not make the
// write fail wholesale.
func enableLine(avail []string) string {
	var parts []string
	for _, want := range wantControllers {
		if contains(avail, want) {
			parts = append(parts, "+"+want)
		}
	}
	return strings.Join(parts, " ")
}

// readControllers reads a space-separated controller list (cgroup.controllers).
func readControllers(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(body)), nil
}

// readCgroupProcs reads the pids in a cgroup.procs file.
func readCgroupProcs(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("executor: caps: reading %s: %w", path, err)
	}
	return strings.Fields(string(body)), nil
}

// readKeyedUint reads a flat keyed file (key value per line) and returns the
// value for key.
func readKeyedUint(path, key string) (uint64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			return strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("executor: caps: key %q not found in %s", key, path)
}

// writeCgroupFile writes a single value to a cgroup control file. cgroup writes
// take no trailing newline and reject O_APPEND, so this is a plain truncating
// write of exactly the bytes given.
func writeCgroupFile(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o644)
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
