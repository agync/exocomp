package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// jobDirPrefix names the per-job private directory the sandboxed tier creates
// inside the private root. Each job gets its own so a grant covers one job's
// material and not the material of every job sharing that root — "other jobs'
// tmpfs" is on ADR 0005's denied list. The name is short for the reason
// newJobDir explains.
const jobDirPrefix = "job-"

// shmDir is the POSIX shared-memory mount ansible's multiprocessing needs.
const shmDir = "/dev/shm"

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
	job, cleanup, err := newJobDir(a.opts.PrivateDirRoot)
	if err != nil {
		return Result{}, err
	}
	defer cleanup(sink)

	opts := a.opts
	opts.PrivateDirRoot = job.private
	// Ansible's own scratch defaults under $HOME and /tmp, and the sandbox
	// grants neither: /tmp is write-only under nono's system profile, so a
	// module that writes a payload there cannot read it back.
	opts.Env = append(slices.Clone(opts.Env),
		"ANSIBLE_LOCAL_TEMP="+job.tmp,
		"ANSIBLE_REMOTE_TMP="+job.tmp,
		"TMPDIR="+job.tmp,
		"HOME="+job.home,
	)

	// ansible reads and writes ~/.ansible on every run — the ssh connection
	// plugin puts its ControlPath sockets under ~/.ansible/cp — and reports a
	// denial there as a task failure. HOME itself stays ungranted (the launcher
	// keeps its own state under the real one), so the single directory ansible
	// needs is created and granted while the rest of the redirected home is not.
	ansibleHome, err := job.homeSubdir(".ansible")
	if err != nil {
		return Result{}, err
	}

	sb := a.sandbox
	// ansible-core forks a worker per host and coordinates them with POSIX
	// semaphores, which live in /dev/shm; without it the run dies at startup
	// with "Unable to use multiprocessing".
	sb.WritePaths = append(slices.Clone(sb.WritePaths), append(job.grants(), shmDir, ansibleHome)...)
	// ssh resolves the local user through /etc/passwd on startup and aborts
	// ("No user exists for uid N") when it cannot read it; nono does not grant
	// it by default. It is a single file, world-readable in the normal case,
	// and holds no secret — the shadow file, which does, is never granted.
	sb.ReadFiles = append(slices.Clone(sb.ReadFiles), passwdFile)
	sb.ReadPaths = append(slices.Clone(sb.ReadPaths), a.opts.CollectionsPath)
	sb.ReadPaths = append(sb.ReadPaths, ansibleSourceDirs(spec.Argv)...)
	sb.ReadPaths = append(sb.ReadPaths, toolGrant(a.opts.Binary, AnsibleBinary)...)
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

	drv := NewAnsible(opts)
	drv.inner = NewSandbox(KindAnsible, sb, drv.inner)
	return drv.Run(ctx, spec, sink)
}

// ansibleSourceDirs are the repository paths an ansible argv names: the
// playbooks it runs and the inventory it reads. ansible-playbook's positional
// arguments are playbook files and nothing else, and the inventory arrives
// through one flag, so the argv says exactly which checkout the run needs —
// there is no guessing and no wider grant than the files named.
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

	for i := 1; i < len(argv); i++ {
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
		case strings.HasPrefix(arg, "--inventory="):
			if v := strings.TrimPrefix(arg, "--inventory="); !strings.HasSuffix(v, ",") {
				add(v)
			}
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
	if port := backendPort(t.opts.BackendURL); port != 0 {
		sb.ConnectPorts = append(slices.Clone(sb.ConnectPorts), port)
	}

	// A `plan -out=` points at the data dir's artifacts tree, which the sandbox
	// denies by design. Redirect it into the job's own private dir, which is
	// granted, then relocate the finished plan to the requested path once the
	// subprocess has exited — this driver runs unsandboxed, so it can write the
	// data dir the job could not.
	planDst, argv := planOutPath(spec.Argv)
	if planDst != "" {
		redirected := filepath.Join(job.private, "plan.tfplan")
		spec.Argv = setPlanOutPath(argv, redirected)
		defer relocatePlan(redirected, planDst, sink)
	}
	// `apply <planfile>` reads a saved plan the caller decrypted into the data
	// dir, which the sandbox denies just the same. Stage it into the granted
	// job dir — the driver reads the data-dir plaintext the subprocess cannot.
	staged, err := stagePlanInput(spec.Argv, job.private)
	if err != nil {
		return Result{}, err
	}
	spec.Argv = staged

	drv := NewTofu(opts)
	drv.inner = NewSandbox(KindTofu, sb, drv.inner)
	return drv.Run(ctx, spec, sink)
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
