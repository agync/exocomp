package deps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// UVBinary is the resolver the control plane invokes for project dependencies.
const UVBinary = "uv"

// manifestEnv lets an operator or the container point the resolver at the image
// toolchain manifest so the venv interpreter tracks a switched image without a
// rebuild of this binary.
const manifestEnv = "EXOCOMP_TOOLCHAIN_MANIFEST"

// readyMarker sits at a venv's root once it is fully provisioned. Its presence
// (carrying the manifest hash) is what makes the cache hash-gated: an existing
// marker means reuse with no uv invocation, while a half-built venv from a
// crashed resolve has none and is rebuilt rather than trusted.
const readyMarker = ".exocomp-venv-ready"

// VenvSpec describes the venv to ensure for one source's manifest.
type VenvSpec struct {
	// CacheRoot is <data dir>/cache/python; the venv lands at
	// CacheRoot/<Source>/<manifest hash>.
	CacheRoot string
	// Source is the safe cache namespace for this project (a source name or a
	// slug of its root). It is a single path component.
	Source string
	// Manifest is the resolved Python manifest; EnsureVenv requires HasDeps.
	Manifest PythonManifest
	// UVBinary overrides the uv executable. Empty resolves uv on PATH.
	UVBinary string
	// Offline passes uv's --offline, so resolution is served from the uv cache
	// alone with no network. Set it once the cache is known to hold the pins.
	Offline bool
	// Sandbox, when set, wraps every uv invocation in the job sandbox tier so
	// the resolver — arbitrary code an untrusted manifest can steer — never
	// runs in the daemon context. Nil runs uv directly, which the
	// hermetic unit tests and a host with no sandbox both rely on.
	Sandbox SandboxLauncher
	// UVCacheDir, when set, is exported as UV_CACHE_DIR so uv's download cache
	// lands on a granted, backed-up path instead of the daemon's home — which
	// the sandbox does not grant. Empty leaves uv's default cache location.
	UVCacheDir string
	// KeepVenvs is the newest-N retention window PruneVenvs applies to this
	// source's other venvs after a successful provision (the venv just built
	// is protected outright, on top of naturally landing in this window).
	// Zero uses DefaultKeepVenvs.
	KeepVenvs int
	// KeepVenvsSince bounds the retention window's other half: a venv built
	// within KeepVenvsSince of now is kept regardless of KeepVenvs. Zero uses
	// DefaultKeepVenvsSince.
	KeepVenvsSince time.Duration
	// Logger receives a warning if the post-provision prune fails; the
	// failure never fails EnsureVenv itself (see pruneStaleVenvs). Nil uses
	// slog.Default().
	Logger *slog.Logger
}

// Venv is a provisioned per-source virtual environment.
type Venv struct {
	// Dir is the venv root, CacheRoot/<Source>/<hash>.
	Dir string
	// Bin is Dir/bin, the directory prepended to PATH for the run.
	Bin string
	// Python is the interpreter ansible_python_interpreter points at for the
	// localhost-over-local run.
	Python string
	// SitePackages is Dir/lib/pythonX.Y/site-packages, the project's installed
	// libraries (Decision 0016). It is bridged to the PATH-resolved fallback ansible
	// through PYTHONPATH.
	SitePackages string
	// Ansible is the venv's ansible-playbook. It exists only when the project
	// declared ansible-core; an absent one leaves the run on the PATH-resolved
	// fallback toolchain (Decision 0016).
	Ansible string
	// Created is true when uv was invoked on this call; false on a cache hit.
	Created bool
}

// EnsureVenv returns the venv for spec.Manifest, provisioning it with uv only
// when the hash-gated marker is absent. On a cache hit it invokes no uv and
// touches no network. It is fail-closed: a provisioning failure removes the
// half-built venv and returns an error, so a run never proceeds against
// half-met dependencies (Decision 0011).
//
// A successful provision also prunes this source's other stale venvs
// (PruneVenvs, keep-policy defaults DefaultKeepVenvs/DefaultKeepVenvsSince,
// overridable via spec), protecting the hash just built — otherwise the
// cache grows one directory per manifest hash forever. Pruning never fails
// this call; see pruneStaleVenvs.
func EnsureVenv(ctx context.Context, spec VenvSpec) (Venv, error) {
	if !spec.Manifest.HasDeps() {
		return Venv{}, errors.New("deps: EnsureVenv called for a source with no declared dependencies")
	}
	if spec.CacheRoot == "" {
		return Venv{}, errors.New("deps: EnsureVenv needs a cache root")
	}
	if !safeComponent(spec.Source) {
		return Venv{}, fmt.Errorf("deps: source %q is not a safe cache component", spec.Source)
	}

	dir := filepath.Join(spec.CacheRoot, spec.Source, spec.Manifest.Hash)
	v := venvPaths(dir)

	if markerMatches(dir, spec.Manifest.Hash) {
		return v, nil
	}

	v, err := provision(ctx, spec, dir, v)
	if err != nil {
		// A partial venv would be reused as if complete on the next run; remove
		// it so the failure is not sticky.
		_ = os.RemoveAll(dir)
		return Venv{}, err
	}
	v.Created = true
	pruneStaleVenvs(spec, spec.Manifest.Hash)
	return v, nil
}

// provision creates the venv and installs the declared deps into it, writing
// the ready marker only after the install exits clean. The interpreter is the
// one the image froze (Decision 0016) and no ansible-core is forced: ansible is a
// command on PATH, and a project that declares its own core shadows it. The
// venv's site-packages is resolved here, after uv has laid the directory down,
// and returned so the caller can bridge it through PYTHONPATH.
func provision(ctx context.Context, spec VenvSpec, dir string, v Venv) (Venv, error) {
	uv := spec.UVBinary
	if uv == "" {
		resolved, err := exec.LookPath(UVBinary)
		if err != nil {
			return Venv{}, fmt.Errorf("deps: %w", err)
		}
		uv = resolved
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Venv{}, fmt.Errorf("deps: cache dir: %w", err)
	}
	// uv creates its download cache subtree itself, but under the sandbox tier a
	// Landlock write grant attaches only to a path that already exists, and uv's
	// own mkdir of the leaf would need write on the ungranted parent cache tree.
	// Create the granted cache dir here in the daemon so uv fills its subtree.
	if spec.UVCacheDir != "" {
		if err := os.MkdirAll(spec.UVCacheDir, 0o700); err != nil {
			return Venv{}, fmt.Errorf("deps: uv cache dir: %w", err)
		}
	}
	// A leftover partial from an earlier failed attempt must not confuse uv.
	if err := os.RemoveAll(dir); err != nil {
		return Venv{}, fmt.Errorf("deps: clear stale venv: %w", err)
	}

	// The resolver runs with its working directory inside the granted cache
	// tree: under the sandbox an ungranted cwd is refused outright, and the
	// per-source cache dir is a write grant the profile already carries.
	work := filepath.Dir(dir)
	venvArgs := []string{"venv"}
	if pin := imagePython(); pin != "" {
		venvArgs = append(venvArgs, "--python", pin)
	}
	venvArgs = append(venvArgs, dir)
	if err := runUV(ctx, uv, spec, work, venvArgs...); err != nil {
		return Venv{}, err
	}
	sp, err := sitePackagesDir(dir)
	if err != nil {
		return Venv{}, err
	}
	v.SitePackages = sp

	install := []string{"pip", "install", "--python", v.Python}
	install = append(install, installArgs(spec.Manifest)...)
	if err := runUV(ctx, uv, spec, work, install...); err != nil {
		return Venv{}, err
	}

	if err := os.WriteFile(v.marker(), []byte(spec.Manifest.Hash+"\n"), 0o600); err != nil {
		return Venv{}, fmt.Errorf("deps: write ready marker: %w", err)
	}
	return v, nil
}

// runUV invokes uv with the resolver environment. Correctness is the exit code
// (AGENTS.md invariant 3); uv's stderr rides in the returned error for the
// operator, never parsed for a verdict. When the spec carries a sandbox the uv
// argv is wrapped in the launcher first, so a poisoned manifest's build backend
// runs confined rather than with daemon privileges.
func runUV(ctx context.Context, uv string, spec VenvSpec, workDir string, args ...string) error {
	if spec.Offline {
		args = append([]string{"--offline"}, args...)
	}
	subcommand := args[0]
	argv := append([]string{uv}, args...)
	if spec.Sandbox != nil {
		wrapped, err := spec.Sandbox.Wrap(argv)
		if err != nil {
			return fmt.Errorf("deps: sandbox uv %s: %w", subcommand, err)
		}
		argv = wrapped
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workDir
	// Hardlink-seed from the shared uv cache so per-source venvs are cheap and
	// offline once cached (Decision 0011); uv falls back to copy across filesystems.
	cmd.Env = append(os.Environ(), "UV_LINK_MODE=hardlink")
	if spec.UVCacheDir != "" {
		cmd.Env = append(cmd.Env, "UV_CACHE_DIR="+spec.UVCacheDir)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("deps: uv %s: %w: %s", subcommand, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// installArgs is the uv-pip tail that installs the declared dependencies:
// -r for a requirements file, the extracted specifiers for a pyproject. uv
// understands the manifest formats natively, so nothing here re-implements
// resolution. The pyproject specifiers are passed after `--`, so a specifier
// beginning with a dash cannot be read as a uv flag; parse-time
// validation already refuses one, and this is the defence-in-depth behind it.
// The content was validated at discovery, so the parse here cannot fail.
func installArgs(m PythonManifest) []string {
	switch m.Kind {
	case KindRequirements:
		return []string{"-r", m.Path}
	case KindPyproject:
		specs, err := parsePyprojectDeps(m.content)
		if err != nil || len(specs) == 0 {
			return nil
		}
		return append([]string{"--"}, specs...)
	default:
		return nil
	}
}

// imagePython resolves the interpreter the image froze into its toolchain
// manifest (Decision 0016), or "" when none is reachable. The fallback ansible
// toolchain and every per-source venv must share one interpreter so the
// site-packages PYTHONPATH bridge is ABI-compatible; an empty pin omits uv's
// --python and leaves the default. A switched image moves the pin with it, no
// rebuild of this binary required.
func imagePython() string {
	path := os.Getenv(manifestEnv)
	if path == "" {
		return ""
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m struct {
		Python string `json:"python"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return m.Python
}

// sitePackagesDir locates the venv's installed-libraries directory. A venv
// carries exactly one lib/pythonX.Y/site-packages; none (uv failed to lay it
// down) or several (an interpreter-level mismatch) is an error rather than a
// guess, so a run never bridges a path that is not the project's own.
func sitePackagesDir(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "lib", "python*", "site-packages"))
	if err != nil {
		return "", fmt.Errorf("deps: glob site-packages under %s: %w", dir, err)
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("deps: venv %s has %d site-packages dirs, want exactly 1", dir, len(matches))
	}
	return matches[0], nil
}

// venvPaths derives the standard interpreter and tool paths under a venv root.
// SitePackages is filled best-effort for a cache hit: provision validates the
// directory before it writes the ready marker, so a marked venv normally
// carries exactly one, and a miss leaves it empty rather than failing a reuse.
func venvPaths(dir string) Venv {
	bin := filepath.Join(dir, "bin")
	v := Venv{
		Dir:     dir,
		Bin:     bin,
		Python:  filepath.Join(bin, "python"),
		Ansible: filepath.Join(bin, "ansible-playbook"),
	}
	if sp, err := sitePackagesDir(dir); err == nil {
		v.SitePackages = sp
	}
	return v
}

func (v Venv) marker() string { return filepath.Join(v.Dir, readyMarker) }

// markerMatches reports whether dir holds a ready marker for the given hash. A
// mismatching marker (a hash collision path reused, or a truncated write) is
// treated as absent so the venv is rebuilt rather than trusted.
func markerMatches(dir, hash string) bool {
	body, err := os.ReadFile(filepath.Join(dir, readyMarker))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(body)) == hash
}

// safeComponent reports whether name is a single safe path component, matching
// the source model's rule so a crafted source name cannot escape the cache.
func safeComponent(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// SourceKey turns an arbitrary project root path into a stable, safe cache
// namespace: the directory's base name slugged, plus a short hash of the full
// path so two projects with the same base name never share a venv namespace.
func SourceKey(root string) string {
	base := slug(filepath.Base(root))
	if base == "" {
		base = "src"
	}
	return base + "-" + shortHash(root)
}

// slug reduces a path component to the safe alphabet, collapsing every other
// rune to a dash so a project directory name becomes a usable cache namespace.
func slug(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// shortHash is a stable short hex digest of s, used to disambiguate two roots
// that slug to the same base name.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}
