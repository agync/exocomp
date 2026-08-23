package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Tier names how much isolation a job subprocess runs under
// (docs/architecture/execution.md).
const (
	// TierLocal is the subprocess-boundary tier: no kernel policy.
	TierLocal Tier = "local"
	// TierLocalSandboxed wraps every job subprocess in a default-deny nono
	// sandbox (ADR 0005).
	TierLocalSandboxed Tier = "local-sandboxed"
	// TierAuto asks for TierLocalSandboxed and accepts a loud degradation to
	// TierLocal when the host cannot enforce it.
	TierAuto Tier = "auto"
)

// Tier is one of the constants above.
type Tier string

// ParseTier reads a tier name. Empty means TierAuto: a host that can sandbox
// does, and one that cannot says so.
func ParseTier(s string) (Tier, error) {
	switch t := Tier(strings.TrimSpace(s)); t {
	case "":
		return TierAuto, nil
	case TierLocal, TierLocalSandboxed, TierAuto:
		return t, nil
	default:
		return "", fmt.Errorf("executor: unknown tier %q (want %s, %s, or %s)",
			s, TierLocal, TierLocalSandboxed, TierAuto)
	}
}

// NonoBinary is the sandbox launcher the local-sandboxed tier runs jobs under.
const NonoBinary = "nono"

// NonoMinVersion is the floor ADR 0005 pins: CVE-2026-47128 — an escape via a
// powerful local unix socket — is fixed in 0.55.0, and that escape class is
// exactly what this project's own sockets would be vulnerable to.
const NonoMinVersion = "0.55.0"

// Ports a job kind is allowed to reach out on. Landlock's network rules filter
// by port, not by destination, so these are the whole story (see
// docs/operations/serve.md).
const (
	// SSHPort is what an ansible job connects to on an inventory host.
	SSHPort = 22
	// HTTPSPort is what a tofu job fetches providers and talks to APIs over.
	HTTPSPort = 443
)

// deniedDataDirNames are the files inside the data dir that no job subprocess
// may reach under any profile. They are checked by name rather than trusted to
// nono's default-deny, because the grants a profile *does* carry all live
// inside the same data dir: one grant written one level too high is the whole
// failure mode.
var deniedDataDirNames = []string{
	"exocomp.db",
	"exocomp.db-wal",
	"exocomp.db-shm",
	"host.key",
	"keyring.age",
	"serve.lock",
	"state-backend.addr",
}

// SandboxOptions is the per-run policy the sandbox enforces. Every path is a
// grant; everything not named here is denied by the kernel, unix sockets
// included — no profile this package builds carries a socket grant, which is
// the CVE lesson ADR 0005 records.
type SandboxOptions struct {
	// Binary overrides the launcher. Empty resolves nono on PATH.
	Binary string
	// DataDir is the 0700 directory holding the database and the keys. It is
	// never granted, and no grant may cover it. Empty fails closed.
	DataDir string
	// ReadPaths are granted read-only: tool trees, vendored collections.
	ReadPaths []string
	// ReadFiles are single files granted read-only, for a system file a tool
	// insists on that no directory grant should widen to — /etc/passwd, which
	// ssh resolves the local user through, above all.
	ReadFiles []string
	// WritePaths are granted read+write: the job's private dir, the workspace
	// checkout, provider caches a run populates.
	WritePaths []string
	// ConnectPorts are the outbound TCP ports the job may connect to. Empty
	// denies egress entirely.
	ConnectPorts []int
}

// SandboxProfile is one kind's resolved default-deny policy.
type SandboxProfile struct {
	Binary       string
	DataDir      string
	Read         []string
	ReadFiles    []string
	Write        []string
	ConnectPorts []int
}

// ProfileFor resolves the policy for one job kind: the caller's grants plus
// the egress each kind cannot work without.
func ProfileFor(kind string, opts SandboxOptions) SandboxProfile {
	p := SandboxProfile{
		Binary:       opts.Binary,
		DataDir:      opts.DataDir,
		Read:         normalizePaths(opts.ReadPaths),
		ReadFiles:    normalizePaths(opts.ReadFiles),
		Write:        normalizePaths(opts.WritePaths),
		ConnectPorts: slices.Clone(opts.ConnectPorts),
	}
	if p.Binary == "" {
		p.Binary = NonoBinary
	}

	switch kind {
	case KindAnsible:
		p.ConnectPorts = append(p.ConnectPorts, SSHPort)
	case KindTofu:
		p.ConnectPorts = append(p.ConnectPorts, HTTPSPort)
	case KindCLI:
		// No egress for the kind, deliberately. ansible and tofu each speak one
		// protocol, so a port belongs to the kind; the CLI kind is every
		// allow-listed binary at once, and a kind-wide grant would hand each of
		// them the widest posture any single registration asked for. A tool that
		// declares egress gets its port added to its own run's profile by
		// NewSandboxedCLI instead.
	}
	slices.Sort(p.ConnectPorts)
	p.ConnectPorts = slices.Compact(p.ConnectPorts)

	// A path granted read+write does not also need the read-only grant, and
	// nono reports the narrower one as insufficient rather than merging them.
	p.Read = slices.DeleteFunc(p.Read, func(r string) bool { return slices.Contains(p.Write, r) })
	return p
}

// normalizePaths cleans, drops empties, and de-duplicates. It deliberately does
// not absolutize: a relative grant would silently mean "wherever this process
// happens to be", and the guard refuses one instead.
func normalizePaths(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" {
			continue
		}
		clean := filepath.Clean(p)
		if !slices.Contains(out, clean) {
			out = append(out, clean)
		}
	}
	slices.Sort(out)
	return out
}

// ToolGrants resolves each named tool on PATH and returns the read grants a
// sandboxed run needs to execute it. A tool is not only its binary: an
// interpreted one — every `uv tool install` shim, ansible-playbook included —
// carries a shebang back into the interpreter and the site-packages beside it,
// so a `bin` directory means the toolchain root above it is granted too. Tools
// that do not resolve are skipped: the driver reports a missing binary far more
// clearly than a sandbox denial would.
func ToolGrants(names ...string) []string {
	var out []string
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		out = append(out, toolDirs(abs)...)
	}
	return normalizePaths(out)
}

// toolDirs is the grant set for one resolved executable: its directory, plus
// the toolchain root above a `bin` when the executable is a script. A compiled
// binary needs nothing but itself, and widening to the parent of a shared
// `bin` — `~/.local` for a tool installed there — would grant far more than
// the tool.
func toolDirs(path string) []string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "bin" && isScript(path) {
		return []string{dir, filepath.Dir(dir)}
	}
	return []string{dir}
}

// isScript reports whether the file starts with a shebang, which is what makes
// the tree beside it — the interpreter and its site-packages — part of the
// tool rather than an unrelated neighbour.
func isScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	var head [2]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	return head[0] == '#' && head[1] == '!'
}

// Command builds the launcher invocation wrapping argv, after checking every
// grant against the data dir. It returns an error rather than a weaker sandbox
// when a grant would expose the database or the keys.
func (p SandboxProfile) Command(argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, errors.New("executor: sandbox: empty argv")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(argv)+2*(len(p.Read)+len(p.ReadFiles)+len(p.Write)+len(p.ConnectPorts))+6)
	// --silent keeps nono's banner out of the job's event stream; --no-audit
	// keeps its own Merkle ledger out of the data volume, because job_event is
	// this project's audit surface and a second one would only rot.
	out = append(out, p.Binary, "run", "--silent", "--no-audit", "--block-net")
	for _, r := range p.Read {
		out = append(out, "--read", r)
	}
	for _, f := range p.ReadFiles {
		out = append(out, "--read-file", f)
	}
	for _, w := range p.Write {
		out = append(out, "--allow", w)
	}
	for _, port := range p.ConnectPorts {
		out = append(out, "--allow-connect-port", strconv.Itoa(port))
	}
	out = append(out, "--")
	return append(out, argv...), nil
}

// Validate is the guard: it refuses a profile whose grants would let a job read
// the database, the host key, the keyring, the daemon lock, or the recorded
// backend address. Grants *inside* the data dir are the normal case — the
// private run dir, the provider cache, the vendored collections all live there
// — so the check is per denied file, not per directory.
func (p SandboxProfile) Validate() error {
	dataDir, err := filepath.Abs(strings.TrimSpace(p.DataDir))
	if p.DataDir == "" || err != nil {
		return errors.New("executor: sandbox: no data dir to protect; refusing to build a profile")
	}

	// Both the data dir and a grant may reach the same bytes through a symlink
	// a lexical compare would miss — a grant on a symlink to the data dir, or a
	// data dir under a symlinked ancestor. Resolve each and check the resolved
	// forms as well; a path that does not yet exist keeps its lexical form.
	realDataDir := resolveSymlinks(dataDir)

	type target struct {
		lexical, resolved string
		isDataDir         bool
	}
	denied := []target{{lexical: dataDir, resolved: realDataDir, isDataDir: true}}
	for _, name := range deniedDataDirNames {
		lex := filepath.Join(dataDir, name)
		denied = append(denied, target{lexical: lex, resolved: filepath.Join(realDataDir, name)})
	}

	for _, grant := range slices.Concat(p.Read, p.ReadFiles, p.Write) {
		if !filepath.IsAbs(grant) {
			return fmt.Errorf("executor: sandbox: grant %q is not absolute", grant)
		}
		grantResolved := resolveSymlinks(grant)
		for _, d := range denied {
			if !covers(grant, d.lexical) && !covers(grantResolved, d.resolved) {
				continue
			}
			if d.isDataDir {
				return fmt.Errorf("executor: sandbox: grant %q is an ancestor of the data dir %s, "+
					"which no job may reach — a source path may not be an ancestor of the data dir; "+
					"keep playbooks, inventories, and workspaces outside it",
					grant, d.lexical)
			}
			return fmt.Errorf("executor: sandbox: grant %q covers %s, which no job may reach",
				grant, d.lexical)
		}
	}
	return nil
}

// resolveSymlinks returns the fully symlink-resolved form of path, or path
// cleaned when it cannot be resolved — a grant that does not exist yet has no
// symlink to follow, and the lexical form is the only honest answer.
func resolveSymlinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// covers reports whether a grant on dir reaches path: the same path, or
// anything beneath it, because every nono path grant is recursive.
func covers(dir, path string) bool {
	dir, path = filepath.Clean(dir), filepath.Clean(path)
	if dir == path {
		return true
	}
	sep := string(filepath.Separator)
	if dir == sep {
		return true
	}
	return strings.HasPrefix(path, dir+sep)
}

// Sandbox runs a spec under the local-sandboxed tier: it rewrites the argv into
// a launcher invocation and hands the result to the driver underneath, so
// streaming, process groups, and exit-code handling keep one implementation.
type Sandbox struct {
	profile SandboxProfile
	inner   Driver
}

// NewSandbox wraps inner in the profile for kind. A nil inner uses the local
// subprocess driver.
func NewSandbox(kind string, opts SandboxOptions, inner Driver) *Sandbox {
	if inner == nil {
		inner = NewShell()
	}
	return &Sandbox{profile: ProfileFor(kind, opts), inner: inner}
}

// Profile returns the resolved policy, for tests and for the doctor.
func (s *Sandbox) Profile() SandboxProfile { return s.profile }

// Run implements Driver.
func (s *Sandbox) Run(ctx context.Context, spec Spec, sink EventSink) (Result, error) {
	profile := s.profile
	if spec.Dir != "" {
		// The launcher refuses a working directory it holds no grant on, and
		// a run's own directory is a grant it always needs.
		profile.Write = normalizePaths(append(slices.Clone(profile.Write), spec.Dir))
		profile.Read = slices.DeleteFunc(profile.Read,
			func(r string) bool { return slices.Contains(profile.Write, r) })
	}

	argv, err := profile.Command(spec.Argv)
	if err != nil {
		return Result{}, err
	}
	spec.Argv = argv
	return s.inner.Run(ctx, spec, sink)
}

// NonoVersion reports the launcher's version string.
func NonoVersion(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		binary = NonoBinary
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("executor: sandbox: %w", err)
	}
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("executor: sandbox: %s --version: %w", path, err)
	}
	// "nono 0.74.0"
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", errors.New("executor: sandbox: empty version output")
	}
	return fields[len(fields)-1], nil
}

// versionAtLeast compares dotted numeric versions, ignoring any pre-release
// suffix. A version that does not parse is treated as too old: a floor that
// fails open is not a floor.
func versionAtLeast(have, floor string) bool {
	parse := func(v string) []int {
		v, _, _ = strings.Cut(v, "-")
		v, _, _ = strings.Cut(v, "+")
		parts := strings.Split(v, ".")
		out := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil
			}
			out = append(out, n)
		}
		return out
	}

	h, f := parse(have), parse(floor)
	if h == nil {
		return false
	}
	for i := range max(len(h), len(f)) {
		var a, b int
		if i < len(h) {
			a = h[i]
		}
		if i < len(f) {
			b = f[i]
		}
		if a != b {
			return a > b
		}
	}
	return true
}

// canaryPrefix labels the one line the canary prints. The exit code is the
// verdict; the line is what keeps a launcher that exited 0 without ever
// reaching the canary from reading as a pass, and it is what carries the
// finding the exit code cannot — see PathnameSocketMediated.
const canaryPrefix = "exocomp-sandbox-canary:"

// Canary exit codes. Anything non-zero fails the probe; the distinct values say
// which assertion the sandbox did not hold up.
const (
	CanaryOK         = 0
	CanaryFileLeaked = 10
	CanarySocketOpen = 11
	CanaryBadInvoke  = 12
	CanaryTCPOpen    = 13
)

// canaryDialTimeout bounds one connect attempt. A sandbox that neither
// connects nor refuses inside this window is not answering.
const canaryDialTimeout = 2 * time.Second

// CanaryTargets are the four things a job must not be able to do, handed to
// the canary as argv so the canary itself holds no policy.
type CanaryTargets struct {
	// KeyPath stands in for host.key: a file outside every grant.
	KeyPath string
	// SocketPath is a listening pathname AF_UNIX socket, the shape of the
	// sockets this project ships.
	SocketPath string
	// AbstractSocket is a listening abstract AF_UNIX socket, without the
	// leading NUL.
	AbstractSocket string
	// TCPAddr is a listening loopback TCP address on a port no profile grants.
	TCPAddr string
}

// ParseCanaryTargets reads the four positional arguments the canary takes.
func ParseCanaryTargets(args []string) (CanaryTargets, error) {
	if len(args) != 4 {
		return CanaryTargets{}, fmt.Errorf(
			"executor: canary needs <key> <socket> <abstract-socket> <tcp-addr>, got %d arguments",
			len(args))
	}
	t := CanaryTargets{KeyPath: args[0], SocketPath: args[1], AbstractSocket: args[2], TCPAddr: args[3]}
	if t.KeyPath == "" || t.SocketPath == "" || t.AbstractSocket == "" || t.TCPAddr == "" {
		return CanaryTargets{}, errors.New("executor: canary targets must all be non-empty")
	}
	return t, nil
}

// RunCanary attempts, from inside the sandbox, exactly what ADR 0005 says a job
// must never manage: read a file standing in for host.key, reach a local unix
// socket, and open a TCP connection the profile does not grant. It prints one
// line naming each outcome and returns 0 only when the three the kernel can
// enforce were refused.
//
// The pathname-socket result is reported rather than enforced. Landlock
// mediates path *opening*, and connect(2) on a pathname AF_UNIX socket opens
// nothing, so no Landlock ABI shipped so far can refuse it; the abstract-socket
// scope, which Landlock does implement, is the one that holds. Failing the tier
// over an assertion no kernel can satisfy would trade a real sandbox for a
// wrong one — the mitigation is that exocomp places no unix socket where a job
// can name it, and this line is how a regression in that would surface.
func RunCanary(out, errOut io.Writer, targets CanaryTargets) int {
	result := func(denied bool) string {
		if denied {
			return "denied"
		}
		return "ALLOWED"
	}

	fileDenied := true
	if body, err := os.ReadFile(targets.KeyPath); err == nil {
		_, _ = fmt.Fprintf(errOut, "%s read %s (%d bytes)\n", canaryPrefix, targets.KeyPath, len(body))
		fileDenied = false
	}
	abstractDenied := dialDenied("unix", "@"+targets.AbstractSocket)
	pathDenied := dialDenied("unix", targets.SocketPath)
	tcpDenied := dialDenied("tcp", targets.TCPAddr)

	_, _ = fmt.Fprintf(out, "%s file=%s abstract-socket=%s pathname-socket=%s tcp=%s\n",
		canaryPrefix, result(fileDenied), result(abstractDenied),
		result(pathDenied), result(tcpDenied))

	switch {
	case !fileDenied:
		return CanaryFileLeaked
	case !abstractDenied:
		return CanarySocketOpen
	case !tcpDenied:
		return CanaryTCPOpen
	default:
		return CanaryOK
	}
}

// dialDenied reports whether the connection was refused.
func dialDenied(network, address string) bool {
	conn, err := net.DialTimeout(network, address, canaryDialTimeout)
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

// ProbeResult is what the doctor prints and what tier selection decides on.
type ProbeResult struct {
	// Requested is the tier that was asked for.
	Requested Tier
	// Effective is the tier that will actually run jobs.
	Effective Tier
	// NonoVersion is the launcher's version, empty when it is missing.
	NonoVersion string
	// LandlockABI is the kernel's Landlock ABI, 0 when unsupported.
	LandlockABI int
	// Enforced is true when the canary proved the denials the kernel can make.
	Enforced bool
	// PathnameSocketMediated is true when a connect to a pathname AF_UNIX
	// socket was also refused. No Landlock ABI shipped so far does this, so
	// false is the expected answer on Linux and is not a degradation; it is
	// the reason no exocomp socket may live where a job can name it.
	PathnameSocketMediated bool
	// Reason explains a degradation, and is empty when there was none.
	Reason string
}

// Degraded reports whether the effective tier is weaker than the one asked for.
func (r ProbeResult) Degraded() bool {
	return r.Requested != TierLocal && r.Effective != TierLocalSandboxed
}

// ProbeOptions configures the startup probe.
type ProbeOptions struct {
	// Binary overrides the launcher. Empty resolves nono on PATH.
	Binary string
	// Canary is the executable the probe runs inside the sandbox. Empty uses
	// this process's own executable, which is what production does.
	Canary string
	// CanaryArgs are prepended to the targets the canary is handed, so the
	// executable can route to its canary subcommand.
	CanaryArgs []string
	// PrivateRoot is where job directories will be created. It is checked
	// against the AF_UNIX path budget below; empty skips that check.
	PrivateRoot string
}

// The sandboxed tier redirects TMPDIR into each job's private directory, and
// the tools that run there coordinate their workers over unix sockets placed
// under it. AF_UNIX addresses are capped at 108 bytes, so a private root deep
// enough to overrun that budget makes every ansible run fail at startup with a
// timeout that says nothing about paths. It is a property of the deployment,
// known before any job runs, so the probe decides it once and degrades loudly
// rather than letting each job discover it.
const (
	unixPathMax = 108
	// jobPathOverhead is "/job-<10 digits>/t", what this package adds.
	jobPathOverhead = 17
	// toolSocketHeadroom is what a tool adds under TMPDIR — ansible-core's RPC
	// host uses CPython multiprocessing's "pymp-XXXXXXXX/listener-XXXXXXXX".
	toolSocketHeadroom = 34
	// MaxPrivateRootLen is the longest private root the tier can support.
	MaxPrivateRootLen = unixPathMax - jobPathOverhead - toolSocketHeadroom
)

// probeTimeout bounds one canary run. A sandbox that has not answered in this
// long is not a sandbox anyone should wait on at boot.
const probeTimeout = 30 * time.Second

// Probe asserts the sandbox rather than assuming it (ADR 0005): it stands up a
// fake host.key, two listening unix sockets, and a listening loopback TCP port,
// runs the canary under a profile that grants none of them, and requires the
// canary to report the refusals. Every target is live — a refusal has to come
// from the kernel policy, not from there being nothing to reach.
func Probe(ctx context.Context, want Tier, opts ProbeOptions) ProbeResult {
	res := ProbeResult{Requested: want, Effective: TierLocal}
	if want == TierLocal {
		return res
	}

	abi, err := landlockABI()
	res.LandlockABI = abi
	if err != nil || abi < 1 {
		res.Reason = fmt.Sprintf("Landlock unavailable on this kernel (%v)", err)
		return res
	}

	version, err := NonoVersion(ctx, opts.Binary)
	if err != nil {
		res.Reason = fmt.Sprintf("%s not usable: %v", NonoBinary, err)
		return res
	}
	res.NonoVersion = version
	if !versionAtLeast(version, NonoMinVersion) {
		res.Reason = fmt.Sprintf("%s %s is below the %s floor ADR 0005 pins",
			NonoBinary, version, NonoMinVersion)
		return res
	}

	if n := len(opts.PrivateRoot); n > MaxPrivateRootLen {
		res.Reason = fmt.Sprintf(
			"job directory root %s is %d bytes; the sandbox redirects TMPDIR under it and "+
				"AF_UNIX addresses cap at %d, so %d is the most a sandboxed job can work with",
			opts.PrivateRoot, n, unixPathMax, MaxPrivateRootLen)
		return res
	}

	report, err := runCanaryProbe(ctx, opts)
	if err != nil {
		res.Reason = err.Error()
		return res
	}

	res.Enforced = true
	res.PathnameSocketMediated = strings.Contains(report, "pathname-socket=denied")
	res.Effective = TierLocalSandboxed
	return res
}

// runCanaryProbe is the assertion itself. It returns the canary's report line.
func runCanaryProbe(ctx context.Context, opts ProbeOptions) (string, error) {
	canary := opts.Canary
	if canary == "" {
		self, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot locate this executable to probe with: %w", err)
		}
		canary = self
	}

	root, err := os.MkdirTemp("", "exocomp-probe-")
	if err != nil {
		return "", fmt.Errorf("probe dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	targets := CanaryTargets{
		KeyPath:        filepath.Join(root, "host.key"),
		SocketPath:     filepath.Join(root, "probe.sock"),
		AbstractSocket: fmt.Sprintf("exocomp-probe-%d", os.Getpid()),
	}
	if err := os.WriteFile(targets.KeyPath, []byte("probe-canary-not-a-real-key\n"), 0o600); err != nil {
		return "", fmt.Errorf("probe key: %w", err)
	}

	for _, l := range []struct{ network, address string }{
		{"unix", targets.SocketPath},
		{"unix", "@" + targets.AbstractSocket},
	} {
		listener, err := net.Listen(l.network, l.address)
		if err != nil {
			return "", fmt.Errorf("probe socket %s: %w", l.address, err)
		}
		defer func() { _ = listener.Close() }()
		go acceptUntilClosed(listener)
	}

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("probe tcp listener: %w", err)
	}
	defer func() { _ = tcp.Close() }()
	go acceptUntilClosed(tcp)
	targets.TCPAddr = tcp.Addr().String()

	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		return "", fmt.Errorf("probe work dir: %w", err)
	}

	// The profile grants the work dir and the canary's own tool paths, and
	// nothing else: root holds the fake key and the socket, so a grant one
	// level up is exactly the mistake the probe exists to catch.
	profile := ProfileFor("probe", SandboxOptions{
		Binary:     opts.Binary,
		DataDir:    root,
		ReadPaths:  toolDirs(canary),
		WritePaths: []string{work},
	})
	argv := append([]string{canary}, opts.CanaryArgs...)
	argv = append(argv, targets.KeyPath, targets.SocketPath, targets.AbstractSocket, targets.TCPAddr)
	full, err := profile.Command(argv)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	cmd.Dir = work
	out, runErr := cmd.Output()

	report := canaryReport(string(out))
	switch {
	case runErr != nil:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			detail := report
			if detail == "" {
				detail = firstLine(exitErr.Stderr)
			}
			return "", fmt.Errorf("canary exited %d under the sandbox: %s",
				exitErr.ExitCode(), detail)
		}
		return "", fmt.Errorf("canary did not run: %w", runErr)
	case report == "":
		return "", errors.New("canary exited 0 without reporting what it was refused")
	}
	return report, nil
}

// canaryReport picks the canary's report line out of its output.
func canaryReport(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, canaryPrefix) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// acceptUntilClosed keeps a probe listener answering so a refused connect is
// the sandbox's doing and nothing else's.
func acceptUntilClosed(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

// backendPort returns the TCP port a state-backend URL names, or 0.
func backendPort(rawURL string) int {
	if rawURL == "" {
		return 0
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err == nil {
			return n
		}
	}
	if u.Scheme == "https" {
		return HTTPSPort
	}
	return 0
}

// tofuEgressPorts is the egress a sandboxed tofu run may open, on top of the
// HTTPS grant ProfileFor(KindTofu) adds for the kind: the loopback state
// backend's port, and any operator-declared extra ports for the workspace. The
// extras are additive and never replace the default grant, so a workspace with
// none declared keeps exactly the 443-and-backend posture. ProfileFor sorts and
// de-duplicates the final set.
func tofuEgressPorts(base []int, backendURL string, extra []int) []int {
	out := slices.Clone(base)
	if port := backendPort(backendURL); port != 0 {
		out = append(out, port)
	}
	return append(out, extra...)
}
