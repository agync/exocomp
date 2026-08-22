package source

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitBinary is the pinned tool this package shells out to.
const GitBinary = "git"

// ForgeHost returns the SSH host a git URL connects to, so a sync can pin that
// host's key from the recorded known_hosts store (ADR 0010, finding S5). It
// returns "" for a URL that reaches no SSH host — a local path, or an explicit
// file/http/git transport — where there is no host key to verify. A port, when
// present, is dropped: the store keys a host by name, matching how it is
// recorded.
func ForgeHost(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(u, "ssh://"); ok {
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			rest = rest[:slash]
		}
		if colon := strings.LastIndexByte(rest, ':'); colon >= 0 {
			rest = rest[:colon]
		}
		return rest
	}
	// An explicit non-SSH transport carries no SSH host key.
	for _, p := range []string{"file://", "http://", "https://", "git://"} {
		if strings.HasPrefix(u, p) {
			return ""
		}
	}
	// scp-like syntax: [user@]host:path — the host ends at the first colon, and
	// that colon must come before any slash (a leading absolute path has none).
	colon := strings.IndexByte(u, ':')
	if colon < 0 {
		return ""
	}
	if slash := strings.IndexByte(u, '/'); slash >= 0 && slash < colon {
		return ""
	}
	host := u[:colon]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	return host
}

// SyncOptions drives one sync of a git source to a pinned ref.
type SyncOptions struct {
	// Root is the checkout directory in the data volume (Locality.Root).
	Root string
	// URL is the clone URL.
	URL string
	// Ref is the branch, tag, or commit the control plane pins to. Required:
	// a run executes exactly this tree, never "whatever HEAD moved to".
	Ref string
	// DeployKeyPEM is the OpenSSH private key opened from the envelope, held
	// only for the length of this call. Empty syncs a public repo.
	DeployKeyPEM []byte
	// KnownHostsFile, when set, turns on strict host-key checking against it;
	// empty accepts a new host key on first contact (TOFU) and is the quick
	// path the path localities never need.
	KnownHostsFile string
	// Signed requires a good signature on the ref before the checkout advances
	// (the ansible-pull trust anchor). A ref that does not verify fails the sync.
	Signed bool
	// AllowedSignersFile is git's SSH allowed-signers file, needed to verify an
	// SSH-signed tag. Empty leaves git to use its own gpg/ssh configuration.
	AllowedSignersFile string
	// Git overrides the binary. Empty resolves git on PATH.
	Git string
}

// SyncResult is what a completed sync landed on.
type SyncResult struct {
	Ref string
	SHA string
}

// Sync brings Root to the pinned ref: it clones on first use, fetches
// afterwards, verifies the ref's signature when required, then checks out the
// ref detached and reports the resolved commit. Correctness is decided by git's
// exit codes; captured output only enriches an error message.
//
// The deploy key is written to a 0700 file for the length of the call and
// removed on return — never left on disk, never placed in argv or the process
// environment beyond GIT_SSH_COMMAND's -i path.
func Sync(ctx context.Context, opts SyncOptions) (SyncResult, error) {
	if opts.Root == "" {
		return SyncResult{}, fmt.Errorf("source: sync: empty checkout root")
	}
	if strings.TrimSpace(opts.Ref) == "" {
		return SyncResult{}, fmt.Errorf("source: sync %s: no ref to pin", opts.Root)
	}
	git := opts.Git
	if git == "" {
		resolved, err := exec.LookPath(GitBinary)
		if err != nil {
			return SyncResult{}, fmt.Errorf("source: sync: %w", err)
		}
		git = resolved
	}

	sshCommand, cleanup, err := opts.sshCommand()
	if err != nil {
		return SyncResult{}, err
	}
	defer cleanup()

	run := func(dir string, args ...string) (string, error) {
		return runGit(ctx, git, dir, sshCommand, args...)
	}

	cloned, err := isGitDir(opts.Root)
	if err != nil {
		return SyncResult{}, err
	}
	if !cloned {
		if err := os.MkdirAll(filepath.Dir(opts.Root), 0o700); err != nil {
			return SyncResult{}, fmt.Errorf("source: sync: sources dir: %w", err)
		}
		if _, err := run("", "clone", "--no-checkout", "--", opts.URL, opts.Root); err != nil {
			return SyncResult{}, err
		}
	} else if _, err := run(opts.Root, "fetch", "--tags", "--prune", "origin"); err != nil {
		return SyncResult{}, err
	}

	if opts.Signed {
		if err := verifyRef(ctx, git, opts, sshCommand); err != nil {
			return SyncResult{}, err
		}
	}

	// Detach onto the pinned ref, then hard-reset so a force-pushed branch or a
	// dirty tree from a prior run cannot leave stray files the run would read.
	if _, err := run(opts.Root, "checkout", "--detach", "--force", opts.Ref); err != nil {
		return SyncResult{}, err
	}
	if _, err := run(opts.Root, "reset", "--hard", opts.Ref); err != nil {
		return SyncResult{}, err
	}
	if _, err := run(opts.Root, "clean", "-fdx"); err != nil {
		return SyncResult{}, err
	}

	sha, err := run(opts.Root, "rev-parse", "HEAD")
	if err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Ref: opts.Ref, SHA: strings.TrimSpace(sha)}, nil
}

// verifyRef requires a good signature on the pinned ref: a signed tag first,
// falling back to a signed commit, so either the ansible-pull tag model or a
// commit-signing workflow satisfies the trust anchor. A ref with no valid
// signature fails the sync — the bad commit cannot reach the fleet.
func verifyRef(ctx context.Context, git string, opts SyncOptions, sshCommand string) error {
	// Fail closed: a --signed source with no exocomp-owned allowed-signers must
	// never fall back to the ambient host git config (finding S2). Whatever keys
	// that config happens to name — a permissive or attacker-influenced
	// allowedSignersFile — would decide the verdict, so a ref an attacker signed
	// gets accepted. Refuse rather than verify against trust exocomp does not own.
	if strings.TrimSpace(opts.AllowedSignersFile) == "" {
		return fmt.Errorf("source: sync %s: --signed requires an exocomp-owned allowed-signers file; "+
			"refusing to verify ref %q against the ambient git configuration", opts.Root, opts.Ref)
	}

	// Pin verification to the SSH signature format and exocomp's own signers,
	// overriding any ambient gpg.format / allowedSignersFile the host config sets.
	config := []string{
		"-c", "gpg.format=ssh",
		"-c", "gpg.ssh.allowedSignersFile=" + opts.AllowedSignersFile,
	}

	tagArgs := append(append([]string{}, config...), "verify-tag", opts.Ref)
	if _, err := runGit(ctx, git, opts.Root, sshCommand, tagArgs...); err == nil {
		return nil
	}
	commitArgs := append(append([]string{}, config...), "verify-commit", opts.Ref)
	if _, err := runGit(ctx, git, opts.Root, sshCommand, commitArgs...); err == nil {
		return nil
	}
	return fmt.Errorf("source: sync %s: ref %q carries no verifiable signature; "+
		"refusing to advance a signed source to an unsigned ref", opts.Root, opts.Ref)
}

// sshCommand writes the deploy key to a 0700 file and returns the
// GIT_SSH_COMMAND that uses it, plus a cleanup that removes the key. An empty
// key returns an empty command and a no-op cleanup: a public repo needs none.
func (opts SyncOptions) sshCommand() (string, func(), error) {
	noop := func() {}
	if len(opts.DeployKeyPEM) == 0 && opts.KnownHostsFile == "" {
		return "", noop, nil
	}

	parts := []string{"ssh", "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes"}
	cleanup := noop
	if len(opts.DeployKeyPEM) > 0 {
		dir, err := os.MkdirTemp("", "exocomp-deploykey-")
		if err != nil {
			return "", noop, fmt.Errorf("source: sync: key dir: %w", err)
		}
		cleanup = func() { _ = os.RemoveAll(dir) }
		keyPath := filepath.Join(dir, "id")
		if err := os.WriteFile(keyPath, opts.DeployKeyPEM, 0o600); err != nil {
			cleanup()
			return "", noop, fmt.Errorf("source: sync: write key: %w", err)
		}
		parts = append(parts, "-i", keyPath)
	}
	if opts.KnownHostsFile != "" {
		parts = append(parts, "-o", "StrictHostKeyChecking=yes",
			"-o", "UserKnownHostsFile="+opts.KnownHostsFile)
	} else {
		parts = append(parts, "-o", "StrictHostKeyChecking=accept-new")
	}
	return strings.Join(parts, " "), cleanup, nil
}

// runGit runs one git command, returning its combined output. A non-zero exit
// is an error carrying that output; success returns stdout. The environment is
// deliberately minimal: git reads no operator config, and GIT_SSH_COMMAND is the
// only channel the deploy key travels.
func runGit(ctx context.Context, git, dir, sshCommand string, args ...string) (string, error) {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, git, full...)
	cmd.Env = gitEnv(sshCommand)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("source: git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// gitEnv is the environment git runs under: PATH and HOME from the parent so ssh
// and the key agent resolve, the terminal prompts disabled so a missing
// credential fails instead of hanging, and GIT_SSH_COMMAND carrying the key.
func gitEnv(sshCommand string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_TERMINAL_PROMPT=0",
	}
	if sshCommand != "" {
		env = append(env, "GIT_SSH_COMMAND="+sshCommand)
	}
	return env
}

// isGitDir reports whether root is already a git working tree.
func isGitDir(root string) (bool, error) {
	info, err := os.Stat(filepath.Join(root, ".git"))
	if err == nil {
		return info.IsDir(), nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("source: stat %s: %w", root, err)
}
