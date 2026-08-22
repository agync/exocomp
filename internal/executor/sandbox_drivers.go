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

	sb := a.sandbox
	// ansible-core forks a worker per host and coordinates them with POSIX
	// semaphores, which live in /dev/shm; without it the run dies at startup
	// with "Unable to use multiprocessing".
	sb.WritePaths = append(slices.Clone(sb.WritePaths), append(job.grants(), shmDir)...)
	sb.ReadPaths = append(slices.Clone(sb.ReadPaths), a.opts.CollectionsPath)
	sb.ReadPaths = append(sb.ReadPaths, ansibleSourceDirs(spec.Argv)...)
	sb.ReadPaths = append(sb.ReadPaths, toolGrant(a.opts.Binary, AnsibleBinary)...)

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
	if port := backendPort(t.opts.BackendURL); port != 0 {
		sb.ConnectPorts = append(slices.Clone(sb.ConnectPorts), port)
	}

	drv := NewTofu(opts)
	drv.inner = NewSandbox(KindTofu, sb, drv.inner)
	return drv.Run(ctx, spec, sink)
}

// NewSandboxedShell wraps the plain local driver for every other kind: no
// egress, and only the grants the caller names.
func NewSandboxedShell(sandbox SandboxOptions) Driver {
	return NewSandbox("shell", sandbox, NewShell())
}

// jobDir is one job's private tree. The job's grants are children of it rather
// than the tree itself, and HOME is a child that is deliberately not granted:
// the launcher keeps its own state under $HOME and refuses to start when a
// grant would let the sandboxed process reach it. Whatever a tool genuinely
// needs from its home is granted one directory at a time.
type jobDir struct {
	root    string
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
		root:    base,
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
