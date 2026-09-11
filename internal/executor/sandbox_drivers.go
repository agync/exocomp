package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// applyJobCaps sets up a per-job cgroup when the sandbox options ask for caps,
// mutating spec so the subprocess is born inside it, and returns the teardown.
// It is fail-closed on both edges ADR 0005's "never silently" cares about: caps
// requested without a delegated controller is refused, and a controller that
// cannot mint the cgroup is refused — a capped job never falls back to running
// uncapped. Empty caps are the common path and cost nothing.
func applyJobCaps(sb SandboxOptions, spec *Spec) (func(EventSink), error) {
	if sb.Caps.Empty() {
		return func(EventSink) {}, nil
	}
	if sb.Cgroup == nil {
		return nil, errors.New("executor: caps: memory/cpu caps were requested but no cgroup " +
			"subtree is delegated to this container; refusing to run the job uncapped — " +
			"enable cgroup delegation (see packaging/quadlet/exocomp.container)")
	}
	jc, err := sb.Cgroup.NewJobCgroup(sb.Caps)
	if err != nil {
		return nil, err
	}
	spec.UseCgroup = true
	spec.CgroupFD = jc.FD()
	return func(sink EventSink) {
		// The exit code is the arbiter (invariant 3); this only names the reason
		// an OOM-killed job died 137, which the exit code alone cannot say.
		if oom, oerr := jc.OOMKilled(); oerr == nil && oom {
			sink.Emit(Event{
				Ts:    time.Now().UTC(),
				Level: LevelSystem,
				Msg:   "job exceeded its memory cap and was OOM-killed",
			})
		}
		if err := jc.Remove(); err != nil {
			sink.Emit(Event{Ts: time.Now().UTC(), Level: LevelSystem, Msg: err.Error()})
		}
	}, nil
}

// runUnsandboxed executes spec through the plain driver a privileged job asked
// for: an explicit, operator-gated opt-out of the kernel sandbox for one run,
// the same unsandboxed path a disabled-tier daemon takes. It is the single
// place a sandboxed driver honours Spec.Privileged, so all three kinds skip the
// sandbox the same way. Caps cannot be enforced without the sandbox
// (applyJobCaps runs only inside it), so a privileged job that also named a
// runner is refused rather than run silently uncapped — ADR 0005's "never
// silently", the same posture requireSandboxedForCaps takes for the disabled
// tier.
func runUnsandboxed(ctx context.Context, plain Driver, caps Caps, spec Spec, sink EventSink) (Result, error) {
	if !caps.Empty() {
		return Result{}, errors.New("executor: a privileged (unsandboxed) run cannot also enforce " +
			"per-job caps: caps need the sandbox; drop --privileged or --runner")
	}
	return plain.Run(ctx, spec, sink)
}

// jobDirPrefix names the per-job private directory the sandboxed tier creates
// inside the private root. Each job gets its own so a grant covers one job's
// material and not the material of every job sharing that root — "other jobs'
// tmpfs" is on ADR 0005's denied list. The name is short for the reason
// newJobDir explains.
const jobDirPrefix = "job-"

// passwdFile is the local user database ssh reads to resolve the running uid.
const passwdFile = "/etc/passwd"

// toolGrant is the read grant for the binary a driver will run: the override
// when the caller named one, otherwise whatever the default name resolves to
// on PATH. A driver grants its own tool rather than leaving it to the caller,
// because a sandbox that cannot execute the tool fails as exit 127 — a
// misconfiguration wearing the costume of a job failure.
func toolGrant(override, defaultName string) []string {
	if override != "" {
		return ToolGrants(override)
	}
	return ToolGrants(defaultName)
}

// sandboxedAnsible runs an ansible job under the local-sandboxed tier.
type sandboxedAnsible struct {
	opts    AnsibleOptions
	sandbox SandboxOptions
}

// NewSandboxedAnsible returns an ansible driver whose subprocess runs under
// nono. It is the same driver as NewAnsible with the sandbox spliced in
// underneath, so vault handling, the environment allowlist, and exit-code
// semantics are the ones already tested.
func NewSandboxedAnsible(opts AnsibleOptions, sandbox SandboxOptions) Driver {
	return &sandboxedAnsible{opts: opts, sandbox: sandbox}
}

// Run implements Driver.
func (a *sandboxedAnsible) Run(ctx context.Context, spec Spec, sink EventSink) (Result, error) {
	if spec.Privileged {
		return runUnsandboxed(ctx, NewAnsible(a.opts), a.sandbox.Caps, spec, sink)
	}
	job, cleanup, err := newJobDir(a.opts.PrivateDirRoot)
	if err != nil {
		return Result{}, err
	}
	defer cleanup(sink)

	opts := a.opts
	opts.PrivateDirRoot = job.private

	// ansible reads and writes ~/.ansible on every run — the ssh connection
	// plugin puts its ControlPath sockets under ~/.ansible/cp — and reports a
	// denial there as a task failure. HOME itself stays ungranted (the launcher
	// keeps its own state under the real one), so the single directory ansible
	// needs is created and granted while the rest of the redirected home is not.
	ansibleHome, err := job.homeSubdir(".ansible")
	if err != nil {
		return Result{}, err
	}
	// The ssh ControlPath directory is pinned explicitly under the granted home
	// rather than left to ansible.cfg. cwd is the source checkout, and a read-only
	// checkout whose ansible.cfg sets a RELATIVE control_path_dir (or local_tmp)
	// would resolve that write under the read-only tree and fail; the env var wins
	// over ansible.cfg, so the sockets land in this granted, writable, short path
	// regardless of what the checkout configures. It stays under ~/.ansible/cp,
	// where the sockets already went by default, so the AF_UNIX 108-byte budget is
	// unchanged.
	controlPath := filepath.Join(ansibleHome, "cp")
	if err := os.Mkdir(controlPath, 0o700); err != nil {
		return Result{}, fmt.Errorf("executor: sandbox: control path dir: %w", err)
	}

	// Ansible's own scratch defaults under $HOME and /tmp, and the sandbox
	// grants neither: /tmp is write-only under nono's system profile, so a
	// module that writes a payload there cannot read it back. The scratch env
	// redirects every temp path ansible uses into the per-job granted tree, so a
	// run whose working directory is a read-only source checkout still writes its
	// temp, remote-temp and control sockets somewhere the kernel allows.
	opts.Env = append(slices.Clone(opts.Env), ansibleScratchEnv(job, controlPath)...)

	sb := a.sandbox
	// A job may declare extra SSH egress ports (a nonstandard inventory port)
	// on top of the kind's port 22. The backing slice is cloned before the
	// append because sb is a.sandbox, which a sibling run sharing this driver
	// reuses; ProfileFor sorts and de-duplicates the final set afterwards.
	sb.ConnectPorts = append(slices.Clone(sb.ConnectPorts), spec.EgressPorts...)
	// ansible-core forks a worker per host and coordinates them with POSIX
	// semaphores, which live in /dev/shm; without it the run dies at startup
	// with "Unable to use multiprocessing".
	sb.WritePaths = append(slices.Clone(sb.WritePaths), append(job.grants(), TmpfsRoot, ansibleHome)...)
	// ssh resolves the local user through /etc/passwd on startup and aborts
	// ("No user exists for uid N") when it cannot read it; nono does not grant
	// it by default. It is a single file, world-readable in the normal case,
	// and holds no secret — the shadow file, which does, is never granted.
	sb.ReadFiles = append(slices.Clone(sb.ReadFiles), passwdFile)
	// A job may declare ssh identity key files it must load (a key stored
	// outside the source checkout). Each is granted read-only as a single file,
	// never as a directory — a `~/.ssh` directory grant would widen to every
	// key and known_hosts beside the one declared. The backing slice is cloned
	// before the append because sb is a.sandbox, which a sibling run sharing
	// this driver reuses.
	sb.ReadFiles = append(slices.Clone(sb.ReadFiles), spec.SSHIdentityFiles...)
	// CollectionsPath is a joined ANSIBLE_COLLECTIONS_PATH value — per-source
	// collections ahead of the shared fallback (ADR 0015). Each component is a
	// real directory, so each is granted individually; the joined string itself
	// names no existing path and granting it would deny the read the run needs.
	sb.ReadPaths = append(slices.Clone(sb.ReadPaths), collectionsReadGrants(a.opts.CollectionsPath)...)
	sb.ReadPaths = append(sb.ReadPaths, ansibleSourceDirs(spec.Argv)...)
	sb.ReadPaths = append(sb.ReadPaths, toolGrant(a.opts.Binary, AnsibleBinary)...)
	// An ad-hoc run executes `ansible`, a sibling of ansible-playbook; grant it
	// too so `exocomp command`/`ansible` resolve their tool under the sandbox.
	sb.ReadPaths = append(sb.ReadPaths, ToolGrants(AnsibleAdhocBinary)...)
	// The per-source uv venv (ADR 0011) holds the interpreter and the project's
	// installed libraries; a module that cannot read them fails, so the venv
	// root is granted read when the run is bound to one. It is a data-volume
	// cache path granted for this run, not added to any global allowlist.
	if a.opts.VenvBin != "" {
		sb.ReadPaths = append(sb.ReadPaths, filepath.Dir(a.opts.VenvBin))
	}

	if spec.Dir == "" {
		spec.Dir = job.work
	}

	capsTeardown, err := applyJobCaps(a.sandbox, &spec)
	if err != nil {
		return Result{}, err
	}
	defer capsTeardown(sink)

	drv := NewAnsible(opts)
	drv.inner = NewSandbox(KindAnsible, sb, drv.inner)
	return drv.Run(ctx, spec, sink)
}

// collectionsReadGrants splits a joined ANSIBLE_COLLECTIONS_PATH value into the
// directories it names, so a sandbox grant covers each real directory instead
// of one nonexistent joined path (ADR 0015).
func collectionsReadGrants(collectionsPath string) []string {
	if collectionsPath == "" {
		return nil
	}
	return strings.Split(collectionsPath, string(os.PathListSeparator))
}

// ansibleScratchEnv redirects every scratch path an ansible run writes into the
// per-job granted tree: local and remote temp, the ssh ControlPath directory,
// TMPDIR, and HOME. Each target is a child of the job dir the sandbox grants
// writable (job.tmp and job.home, plus controlPath under the granted
// ~/.ansible), and none is under the run's working directory — so when that
// working directory is a read-only source checkout, a relative local_tmp or
// control_path_dir in the checkout's ansible.cfg cannot force a write onto the
// read-only tree: these env vars take precedence over ansible.cfg.
func ansibleScratchEnv(job jobDir, controlPath string) []string {
	return []string{
		"ANSIBLE_LOCAL_TEMP=" + job.tmp,
		"ANSIBLE_REMOTE_TMP=" + job.tmp,
		"ANSIBLE_SSH_CONTROL_PATH_DIR=" + controlPath,
		"TMPDIR=" + job.tmp,
		"HOME=" + job.home,
	}
}

// ansibleSourceDirs are the repository paths an ansible argv names: the
// playbooks it runs, the inventory it reads, and any `-e @file` extra-vars
// file. ansible-playbook's positional arguments are playbook files and
// nothing else, and the inventory and extra-vars file each arrive through one
// flag, so the argv says exactly which checkout the run needs — there is no
// guessing and no wider grant than the files named.
//
// An ad-hoc run (argv[0] resolves to AnsibleAdhocBinary) carries a host
// PATTERN as its sole positional, argv[1] — a host, a group, or an expression
// like `fedora:!nas` — never a filesystem path, so it is skipped rather than
// scanned: a path-shaped pattern such as `/` must not read-grant the root it
// happens to resemble.
func ansibleSourceDirs(argv []string) []string {
	var dirs []string
	add := func(p string) {
		if !filepath.IsAbs(p) {
			return
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			dirs = append(dirs, p)
			return
		}
		dirs = append(dirs, filepath.Dir(p))
	}
	// addExtraVarsFile grants the file behind an `-e`/`--extra-vars` value
	// when it names one (an "@path" reference); a "key=val" or inline
	// JSON/YAML value names no file and is left alone.
	addExtraVarsFile := func(v string) {
		if p, ok := strings.CutPrefix(v, "@"); ok {
			add(p)
		}
	}

	start := 1
	if len(argv) > 0 && filepath.Base(argv[0]) == AnsibleAdhocBinary {
		start = 2
	}

	for i := start; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-i" || arg == "--inventory" || arg == "--inventory-file":
			if i+1 < len(argv) {
				i++
				// "localhost," and other inline inventories are not paths.
				if !strings.HasSuffix(argv[i], ",") {
					add(argv[i])
				}
			}
		case arg == "-e" || arg == "--extra-vars":
			if i+1 < len(argv) {
				i++
				addExtraVarsFile(argv[i])
			}
		case strings.HasPrefix(arg, "--inventory="):
			if v := strings.TrimPrefix(arg, "--inventory="); !strings.HasSuffix(v, ",") {
				add(v)
			}
		case strings.HasPrefix(arg, "--extra-vars="):
			addExtraVarsFile(strings.TrimPrefix(arg, "--extra-vars="))
		case strings.HasPrefix(arg, "-"):
			// Flags this driver does not interpret; their values are never
			// repository paths the run has to read.
		default:
			add(arg)
		}
	}
	return dirs
}

// sandboxedTofu runs a tofu job under the local-sandboxed tier.
type sandboxedTofu struct {
	opts    TofuOptions
	sandbox SandboxOptions
}

// NewSandboxedTofu returns an OpenTofu driver whose subprocesses run under
// nono, with the loopback state backend's port and HTTPS as the only egress.
func NewSandboxedTofu(opts TofuOptions, sandbox SandboxOptions) Driver {
	return &sandboxedTofu{opts: opts, sandbox: sandbox}
}

// Run implements Driver.
func (t *sandboxedTofu) Run(ctx context.Context, spec Spec, sink EventSink) (Result, error) {
	if spec.Privileged {
		return runUnsandboxed(ctx, NewTofu(t.opts), t.sandbox.Caps, spec, sink)
	}
	job, cleanup, err := newJobDir(t.opts.PrivateDirRoot)
	if err != nil {
		return Result{}, err
	}
	defer cleanup(sink)

	opts := t.opts
	opts.PrivateDirRoot = job.private
	// Provider downloads stage through a temp file that has to be read back,
	// and the system temp dir is write-only under the sandbox by design.
	opts.Env = append(slices.Clone(opts.Env),
		"TMPDIR="+job.tmp,
		"HOME="+job.home,
	)
	// tofu reads ~/.terraform.d on every command and reports a denial as a
	// CLI-configuration problem, so the one directory it looks for exists and
	// is granted while the rest of the redirected home is not.
	tfConfig, err := job.homeSubdir(".terraform.d")
	if err != nil {
		return Result{}, err
	}

	sb := t.sandbox
	sb.WritePaths = append(slices.Clone(sb.WritePaths), append(job.grants(), tfConfig)...)
	sb.ReadPaths = append(slices.Clone(sb.ReadPaths), toolGrant(t.opts.Binary, TofuBinary)...)

	// A run-secret bound to reach the tool as a file (KUBECONFIG, an
	// exec-auth config) is staged here, before the profile is built, so its
	// staging-backend path can be granted to the sandbox — a sandboxed child
	// cannot open a path nono was never told to allow. Staged before
	// NewTofu(opts) runs and RunSecretFiles cleared on opts so the inner,
	// unsandboxed Run does not try to stage it again from a value that is now
	// a path.
	if len(t.opts.RunSecretFiles) > 0 {
		rs, staged, err := StageRunSecretFiles(t.opts.RunSecrets, t.opts.RunSecretFiles)
		if err != nil {
			return Result{}, err
		}
		defer func() {
			for i := range staged {
				_ = staged[i].Close()
			}
		}()
		noteDiskStagingResidual(sink, staged)
		opts.RunSecrets = rs
		opts.RunSecretFiles = nil
		for _, sf := range staged {
			sb.ReadFiles = append(slices.Clone(sb.ReadFiles), sf.Path)
		}
	}
	if t.opts.PluginCacheDir != "" {
		// An online init downloads into the cache, so read-only is not enough.
		sb.WritePaths = append(sb.WritePaths, t.opts.PluginCacheDir)
	}
	// The persistent module cache (ADR 0011) is written by init through the
	// symlink the driver plants in TF_DATA_DIR, so the target itself must be a
	// granted write path or the redirected write is denied.
	if cache := moduleCacheDir(t.opts); cache != "" {
		sb.WritePaths = append(sb.WritePaths, cache)
	}
	sb.ConnectPorts = tofuEgressPorts(sb.ConnectPorts, t.opts.BackendURL, t.opts.ExtraPorts)

	// A `plan -out=` points at the data dir's artifacts tree, which the sandbox
	// denies by design. Redirect it into the job's own private dir, which is
	// granted, then relocate the finished plan to the requested path once the
	// subprocess has exited — this driver runs unsandboxed, so it can write the
	// data dir the job could not. The requested path travels in the job's own
	// argv (queue.Spec, persisted independent of this driver), so it is
	// checked against the artifacts tree before relocation: this driver's
	// unsandboxed write must never land somewhere a crafted -out= chose.
	planDst, argv := planOutPath(spec.Argv)
	if planDst != "" {
		if err := validatePlanArtifactPath(t.sandbox.DataDir, planDst); err != nil {
			return Result{}, err
		}
		redirected := filepath.Join(job.private, "plan.tfplan")
		spec.Argv = setPlanOutPath(argv, redirected)
		defer relocatePlan(redirected, planDst, sink)
		// The driver's own reportPlanOutcome writes the plan's show-json beside
		// the redirected plan file (it reads the run env while it is still
		// live); relocate that companion the same way so the sealer finds both
		// beside the planned -out path. A missing source is a no-op, exactly as
		// for the plan file itself.
		defer relocatePlan(redirected+".show.json", planDst+".show.json", sink)
	}
	// `apply <planfile>` reads a saved plan the caller decrypted into the data
	// dir, which the sandbox denies just the same. Stage it into the granted
	// job dir — the driver reads the data-dir plaintext the subprocess cannot.
	staged, err := stagePlanInput(spec.Argv, job.private)
	if err != nil {
		return Result{}, err
	}
	spec.Argv = staged

	capsTeardown, err := applyJobCaps(t.sandbox, &spec)
	if err != nil {
		return Result{}, err
	}
	defer capsTeardown(sink)

	drv := NewTofu(opts)
	drv.inner = NewSandbox(KindTofu, sb, drv.inner)
	return drv.Run(ctx, spec, sink)
}

// sandboxedCLI runs a governed CLI job under the local-sandboxed tier.
type sandboxedCLI struct {
	opts    CLIOptions
	sandbox SandboxOptions
}

// NewSandboxedCLI returns a CLI driver whose subprocess runs under nono. It is
// the same driver as NewCLI with the sandbox spliced in underneath, so the
// allow-list check, the environment allowlist, the run-secret injection, and
// exit-code semantics are the ones already tested.
func NewSandboxedCLI(opts CLIOptions, sandbox SandboxOptions) Driver {
	return &sandboxedCLI{opts: opts, sandbox: sandbox}
}

// Run implements Driver.
func (c *sandboxedCLI) Run(ctx context.Context, spec Spec, sink EventSink) (Result, error) {
	if spec.Privileged {
		return runUnsandboxed(ctx, NewCLI(c.opts), c.sandbox.Caps, spec, sink)
	}
	job, cleanup, err := newJobDir(c.opts.PrivateDirRoot)
	if err != nil {
		return Result{}, err
	}
	defer cleanup(sink)

	opts := c.opts
	opts.PrivateDirRoot = job.private
	// The system temp dir is write-only under the sandbox and the real home is
	// ungranted, so a tool staging a file or looking for a config directory gets
	// the job's own tree for both. The home is empty by construction: a tool
	// needing configuration takes it from the source checkout or from a bound
	// run-secret, never from the operator's own dotfiles.
	opts.Env = append(slices.Clone(opts.Env),
		"TMPDIR="+job.tmp,
		"HOME="+job.home,
	)

	if spec.Dir == "" {
		spec.Dir = job.work
	}

	capsTeardown, err := applyJobCaps(c.sandbox, &spec)
	if err != nil {
		return Result{}, err
	}
	defer capsTeardown(sink)

	drv := NewCLI(opts)
	drv.inner = NewSandbox(KindCLI, cliSandboxOptions(c.sandbox, c.opts, job), drv.inner)
	return drv.Run(ctx, spec, sink)
}

// cliSandboxOptions is what one governed CLI run may touch: its own private,
// work, and temp directories, and the tool tree the registered binary resolves
// to. Nothing else — the source checkout, when a run names one, arrives as the
// spec's working directory and is granted by Sandbox.Run, so a run with no
// source reaches no repository at all.
//
// Egress is per registration rather than per kind: only a tool that declared
// https gets a port, and it gets exactly 443.
func cliSandboxOptions(base SandboxOptions, opts CLIOptions, job jobDir) SandboxOptions {
	sb := base
	sb.WritePaths = append(slices.Clone(base.WritePaths), job.grants()...)
	sb.ReadPaths = append(slices.Clone(base.ReadPaths), toolGrant(opts.Binary, opts.Tool)...)
	if opts.AllowHTTPS {
		sb.ConnectPorts = append(slices.Clone(base.ConnectPorts), HTTPSPort)
	}
	return sb
}

// planOutPath returns the tofu plan output path named in a `-out=PATH` or
// `-out PATH` argument, or "" when the argv writes no plan file.
func planOutPath(argv []string) (string, []string) {
	for i, a := range argv {
		if v, ok := strings.CutPrefix(a, "-out="); ok {
			return v, argv
		}
		if a == "-out" && i+1 < len(argv) {
			return argv[i+1], argv
		}
	}
	return "", argv
}

// setPlanOutPath rewrites the plan output path in a tofu argv to dst, returning
// a copy so the caller's slice is left alone.
func setPlanOutPath(argv []string, dst string) []string {
	out := slices.Clone(argv)
	for i, a := range out {
		if strings.HasPrefix(a, "-out=") {
			out[i] = "-out=" + dst
			return out
		}
		if a == "-out" && i+1 < len(out) {
			out[i+1] = dst
			return out
		}
	}
	return out
}

// stagePlanInput copies a saved plan named on an `apply <planfile>` argv into
// the granted job dir and rewrites the argv to read it from there. The caller
// decrypts the plan into the data dir, which the sandbox denies; this driver
// runs unsandboxed and can bridge it. An argv that names no plan file — a bare
// `apply -auto-approve`, or any other operation — is returned unchanged.
func stagePlanInput(argv []string, jobPrivate string) ([]string, error) {
	if len(argv) < 2 || argv[1] != "apply" {
		return argv, nil
	}
	for i := 2; i < len(argv); i++ {
		arg := argv[i]
		if strings.HasPrefix(arg, "-") {
			continue
		}
		info, err := os.Stat(arg)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		body, err := os.ReadFile(arg)
		if err != nil {
			return nil, fmt.Errorf("executor: sandbox: read plan %s: %w", arg, err)
		}
		staged := filepath.Join(jobPrivate, "saved.tfplan")
		if err := os.WriteFile(staged, body, 0o600); err != nil {
			return nil, fmt.Errorf("executor: sandbox: stage plan: %w", err)
		}
		out := slices.Clone(argv)
		out[i] = staged
		return out, nil
	}
	return argv, nil
}

// relocatePlan moves a finished plan file from the sandboxed job dir to the
// path the caller will seal it from. The job dir is removed when the job ends,
// so the plan has to leave it first. A missing source is not an error: a failed
// init, or a plan tofu never got to write, simply has none.
func relocatePlan(src, dst string, sink EventSink) {
	if _, err := os.Stat(src); err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		sink.Emit(planRelocateFailed(dst, err))
		return
	}
	if err := os.Rename(src, dst); err == nil {
		return
	}
	// A rename across filesystems fails with a link error; copy then remove.
	body, err := os.ReadFile(src)
	if err != nil {
		sink.Emit(planRelocateFailed(dst, err))
		return
	}
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		sink.Emit(planRelocateFailed(dst, err))
		return
	}
	_ = os.Remove(src)
}

// tofuArtifactsSubdir is the data-dir subtree a tofu plan artifact must
// resolve within.
const tofuArtifactsSubdir = "artifacts"

// validatePlanArtifactPath confirms dst resolves inside dataDir's artifacts
// tree, symlinks resolved on the deepest existing ancestor so a symlink
// planted inside the tree cannot point the relocation back out of it. dataDir
// (SandboxOptions.DataDir) holds the database and keys and is never granted
// to a sandboxed job; this check runs before relocation, in the unsandboxed
// driver, so a -out= naming a traversal never gets the chance to land there
// or anywhere else outside the artifacts tree.
func validatePlanArtifactPath(dataDir, dst string) error {
	if dataDir == "" {
		return errors.New("executor: sandbox: tofu plan relocation: no data dir configured")
	}
	root := filepath.Join(dataDir, tofuArtifactsSubdir)
	realRoot, err := resolvePathAncestor(root)
	if err != nil {
		return fmt.Errorf("executor: sandbox: tofu plan relocation: %w", err)
	}
	target, err := resolvePathAncestor(dst)
	if err != nil {
		return fmt.Errorf("executor: sandbox: tofu plan relocation: %w", err)
	}
	rel, err := filepath.Rel(realRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("executor: sandbox: tofu plan -out %q escapes the artifacts tree", dst)
	}
	return nil
}

// resolvePathAncestor resolves symlinks on the deepest existing ancestor of
// path and re-appends the rest, so a not-yet-created plan file still gets a
// real ancestor to compare against.
func resolvePathAncestor(path string) (string, error) {
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("cannot resolve %q", path)
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}

func planRelocateFailed(dst string, err error) Event {
	return Event{
		Ts:    time.Now().UTC(),
		Level: LevelSystem,
		Msg:   fmt.Sprintf("plan artifact not moved to %s: %v", dst, err),
	}
}

// jobDir is one job's private tree. The job's grants are children of it rather
// than the tree itself, and HOME is a child that is deliberately not granted:
// the launcher keeps its own state under $HOME and refuses to start when a
// grant would let the sandboxed process reach it. Whatever a tool genuinely
// needs from its home is granted one directory at a time.
type jobDir struct {
	private string // driver material: vault password, backend config
	work    string // the working directory when the spec names none
	tmp     string // TMPDIR
	home    string // HOME — created, not granted
}

// grants are the directories the job is allowed to read and write.
func (d jobDir) grants() []string { return []string{d.private, d.work, d.tmp} }

// homeSubdir creates one directory inside the redirected home for a tool that
// insists on reading it, and returns the path to grant.
func (d jobDir) homeSubdir(name string) (string, error) {
	path := filepath.Join(d.home, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", fmt.Errorf("executor: sandbox: %s: %w", path, err)
	}
	return path, nil
}

// newJobDir builds the tree, all 0700, and returns the cleanup that removes it.
//
// The names are one letter for a reason. Tools that coordinate their own
// workers over a unix socket — ansible-core's RPC host, OpenTofu's provider
// plugins — put that socket under TMPDIR, and AF_UNIX addresses are capped at
// 108 bytes: a few more characters here become a startup failure there,
// reported as a timeout that says nothing about paths.
func newJobDir(root string) (jobDir, func(EventSink), error) {
	base, err := os.MkdirTemp(root, jobDirPrefix)
	if err != nil {
		return jobDir{}, nil, fmt.Errorf("executor: sandbox: job dir: %w", err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		_ = os.RemoveAll(base)
		return jobDir{}, nil, fmt.Errorf("executor: sandbox: job dir mode: %w", err)
	}

	d := jobDir{
		private: filepath.Join(base, "p"),
		work:    filepath.Join(base, "w"),
		tmp:     filepath.Join(base, "t"),
		home:    filepath.Join(base, "h"),
	}
	for _, dir := range append(d.grants(), d.home) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			_ = os.RemoveAll(base)
			return jobDir{}, nil, fmt.Errorf("executor: sandbox: %s: %w", dir, err)
		}
	}

	return d, func(sink EventSink) {
		if rmErr := os.RemoveAll(base); rmErr != nil {
			sink.Emit(Event{
				Ts:    time.Now().UTC(),
				Level: LevelSystem,
				Msg:   fmt.Sprintf("job dir %s not removed: %v", base, rmErr),
			})
		}
	}, nil
}
