package deps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// UVBinary is the resolver the control plane invokes for project dependencies.
const UVBinary = "uv"

// defaultAnsibleCore is the fallback ansible-core pin when the image manifest
// cannot be read. It mirrors the Containerfile's ANSIBLE_CORE_VERSION (ADR
// 0006); the venv default tracks the image toolchain so a source that pins
// nothing gets the same core it would have run on the bare image.
const defaultAnsibleCore = "2.21.3"

// defaultAnsibleRunner mirrors the image's ansible-runner pin. ansible-runner
// rides in every venv because the execution model runs plays through it.
const defaultAnsibleRunner = "2.4.3"

// manifestEnv lets an operator or the container point the resolver at the image
// toolchain manifest so the venv default pin tracks a switched image without a
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
	// AnsibleCore overrides the ansible-core pin. Empty uses the image default.
	AnsibleCore string
	// UVBinary overrides the uv executable. Empty resolves uv on PATH.
	UVBinary string
	// Offline passes uv's --offline, so resolution is served from the uv cache
	// alone with no network. Set it once the cache is known to hold the pins.
	Offline bool
}

// Venv is a provisioned per-source virtual environment.
type Venv struct {
	// Dir is the venv root, CacheRoot/<Source>/<hash>.
	Dir string
	// Bin is Dir/bin, the directory prepended to PATH for the run.
	Bin string
	// Python is the interpreter ansible_python_interpreter points at.
	Python string
	// Ansible is the venv's ansible-playbook, the binary the run executes.
	Ansible string
	// Created is true when uv was invoked on this call; false on a cache hit.
	Created bool
}

// EnsureVenv returns the venv for spec.Manifest, provisioning it with uv only
// when the hash-gated marker is absent. On a cache hit it invokes no uv and
// touches no network. It is fail-closed: a provisioning failure removes the
// half-built venv and returns an error, so a run never proceeds against
// half-met dependencies (ADR 0011).
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

	if err := provision(ctx, spec, dir, v); err != nil {
		// A partial venv would be reused as if complete on the next run; remove
		// it so the failure is not sticky.
		_ = os.RemoveAll(dir)
		return Venv{}, err
	}
	v.Created = true
	return v, nil
}

// provision creates the venv and installs the toolchain and declared deps into
// it, writing the ready marker only after the install exits clean.
func provision(ctx context.Context, spec VenvSpec, dir string, v Venv) error {
	uv := spec.UVBinary
	if uv == "" {
		resolved, err := exec.LookPath(UVBinary)
		if err != nil {
			return fmt.Errorf("deps: %w", err)
		}
		uv = resolved
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("deps: cache dir: %w", err)
	}
	// A leftover partial from an earlier failed attempt must not confuse uv.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("deps: clear stale venv: %w", err)
	}

	if err := runUV(ctx, uv, spec, "venv", dir); err != nil {
		return err
	}

	install := []string{"pip", "install", "--python", v.Python}
	install = append(install, toolchainPins(spec)...)
	install = append(install, installArgs(spec.Manifest)...)
	if err := runUV(ctx, uv, spec, install...); err != nil {
		return err
	}

	if err := os.WriteFile(v.marker(), []byte(spec.Manifest.Hash+"\n"), 0o600); err != nil {
		return fmt.Errorf("deps: write ready marker: %w", err)
	}
	return nil
}

// runUV invokes uv with the resolver environment. Correctness is the exit code
// (AGENTS.md invariant 3); uv's stderr rides in the returned error for the
// operator, never parsed for a verdict.
func runUV(ctx context.Context, uv string, spec VenvSpec, args ...string) error {
	if spec.Offline {
		args = append([]string{"--offline"}, args...)
	}
	cmd := exec.CommandContext(ctx, uv, args...)
	// Hardlink-seed from the shared uv cache so per-source venvs are cheap and
	// offline once cached (ADR 0011); uv falls back to copy across filesystems.
	cmd.Env = append(os.Environ(), "UV_LINK_MODE=hardlink")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("deps: uv %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// toolchainPins is the ansible-core + ansible-runner the venv always carries.
// The core default tracks the image (ADR 0006); the moment a project declares
// its own ansible-core in the manifest, uv's resolver takes that pin because
// the declared spec is installed alongside and wins.
func toolchainPins(spec VenvSpec) []string {
	core := spec.AnsibleCore
	if core == "" {
		core = imageAnsibleCore()
	}
	return []string{"ansible-core==" + core, "ansible-runner==" + defaultAnsibleRunner}
}

// installArgs is the uv-pip tail that installs the declared dependencies:
// -r for a requirements file, the extracted specifiers for a pyproject. uv
// understands the manifest formats natively, so nothing here re-implements
// resolution.
func installArgs(m PythonManifest) []string {
	switch m.Kind {
	case KindRequirements:
		return []string{"-r", m.Path}
	case KindPyproject:
		specs, _ := parsePyprojectDeps(m.content)
		return specs
	default:
		return nil
	}
}

// imageAnsibleCore resolves the default ansible-core pin from the image
// toolchain manifest when one is reachable, falling back to the compiled
// default. Reading it means a switched image (ADR 0006) moves the venv default
// with it, no rebuild of this binary required.
func imageAnsibleCore() string {
	path := os.Getenv(manifestEnv)
	if path == "" {
		return defaultAnsibleCore
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return defaultAnsibleCore
	}
	var m struct {
		AnsibleCore string `json:"ansible_core"`
	}
	if err := json.Unmarshal(body, &m); err != nil || m.AnsibleCore == "" {
		return defaultAnsibleCore
	}
	return m.AnsibleCore
}

// venvPaths derives the standard interpreter and tool paths under a venv root.
func venvPaths(dir string) Venv {
	bin := filepath.Join(dir, "bin")
	return Venv{
		Dir:     dir,
		Bin:     bin,
		Python:  filepath.Join(bin, "python"),
		Ansible: filepath.Join(bin, "ansible-playbook"),
	}
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
