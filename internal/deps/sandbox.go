package deps

import (
	"os"
	"path/filepath"
	"slices"

	"exocomp.dev/exocomp/internal/executor"
)

// SandboxLauncher wraps a resolver argv in a confinement launcher. EnsureVenv
// calls it for every uv invocation when a spec carries one, so the resolver
// runs under the job sandbox tier rather than the daemon context.
// A poisoned pyproject.toml or requirements.txt makes uv run a build backend —
// arbitrary code derived from an untrusted source — and that code must be
// denied the data dir, the keys, and the hosts the daemon can otherwise reach.
type SandboxLauncher interface {
	// Wrap returns argv rewritten to run under the launcher, or an error when
	// the profile is unsafe (a grant that would expose the data dir).
	Wrap(argv []string) ([]string, error)
}

// PythonIndexHosts are the hosts uv reaches for the default index. uv does not
// look names up through the system resolver, and the sandbox blocks the lookup it
// does, so a raw port grant cannot serve it; nono's proxy resolves names for it,
// and only for the hosts named.
func PythonIndexHosts() []string {
	return []string{"pypi.org", "files.pythonhosted.org"}
}

// GalaxyHosts are the hosts ansible-galaxy reaches: the galaxy API, and the
// bucket its artifact downloads redirect to.
func GalaxyHosts() []string {
	return []string{"galaxy.ansible.com", "ansible-galaxy-ng.s3.dualstack.us-east-1.amazonaws.com"}
}

// depsSandboxKind names the resolver's sandbox profile. It is deliberately not
// KindAnsible or KindTofu: the resolver's only egress is HTTP(S) to a package
// index through nono's proxy, granted explicitly below, never a per-kind default.
const depsSandboxKind = "deps"

// ResolverSandboxOptions are the grants one resolver run needs. Everything not
// named here is denied by the kernel; the data dir is named so the executor's
// profile guard refuses any grant that would reach the database or the keys.
type ResolverSandboxOptions struct {
	// Binary overrides the nono launcher. Empty resolves it on PATH.
	Binary string
	// DataDir is the 0700 data dir. It is never granted and no grant may cover
	// it — the guard denies host.key, keyring.age, and the database beneath it.
	DataDir string
	// VenvDir is the per-source cache directory uv creates the venv under,
	// granted read+write. It lives inside the data dir's cache tree, which the
	// guard allows while denying the keys beside it.
	VenvDir string
	// ReadPaths are granted read-only: uv's own tool tree, the manifest
	// directory a `-r requirements.txt` install reads, the offline uv cache.
	ReadPaths []string
	// WritePaths are granted read+write on top of VenvDir: the uv cache a
	// download stages through.
	WritePaths []string
	// AllowDomains are the package index hosts the resolver reaches through
	// nono's proxy. Empty reaches no host, which is also what Offline asks for.
	AllowDomains []string
	// Offline drops the network grant: the resolver resolves from the cache
	// alone and reaches no host, whatever AllowDomains names.
	Offline bool
}

// NewResolverSandbox builds the resolver's confinement from the executor's
// job-sandbox machinery (Decision 0005) — the same launcher every job runs under,
// not a second sandbox. uv is granted write on its venv and cache, read on its
// tool tree and the manifest, the proxy only when it must download, and nothing
// else; the data dir and the keys are denied by the guard ProfileFor runs.
func NewResolverSandbox(opts ResolverSandboxOptions) SandboxLauncher {
	write := opts.WritePaths
	if opts.VenvDir != "" {
		write = append([]string{opts.VenvDir}, write...)
	}
	var domains []string
	if !opts.Offline {
		domains = opts.AllowDomains
	}
	profile := executor.ProfileFor(depsSandboxKind, executor.SandboxOptions{
		Binary:       opts.Binary,
		DataDir:      opts.DataDir,
		ReadPaths:    append(slices.Clone(opts.ReadPaths), executor.PythonInstallGrants(opts.DataDir)...),
		ReadFiles:    uvConfigFiles(),
		WritePaths:   write,
		AllowDomains: domains,
	})
	return resolverSandbox{profile: profile}
}

// uvConfigFiles returns the uv configuration files that exist on the host, so
// the confined resolver can read them instead of dying with EACCES. uv's config
// discovery is mandatory and treats an unreadable config as fatal (exit 2,
// "failed to open file `/etc/uv/uv.toml`: Permission denied"), so the
// default-deny profile must grant the system and user uv.toml the operator
// already trusts (host root is out of scope, threat-model). Only existing files
// are granted — a grant on a missing path is not one the launcher should carry.
func uvConfigFiles() []string {
	candidates := []string{"/etc/uv/uv.toml"}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "uv", "uv.toml"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "uv", "uv.toml"))
	}
	var out []string
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

// resolverSandbox adapts an executor profile to SandboxLauncher.
type resolverSandbox struct{ profile executor.SandboxProfile }

// Wrap implements SandboxLauncher.
func (r resolverSandbox) Wrap(argv []string) ([]string, error) {
	return r.profile.Command(argv)
}
