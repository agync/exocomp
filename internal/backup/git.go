package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"filippo.io/age"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
)

// backupRef is the single ref the git leg force-pushes. Each run replaces it
// with one orphan commit, so yesterday's objects become unreachable and git
// can reclaim them — a leg that appended would grow forever.
const backupRef = "refs/heads/backup"

// uriUserinfo matches the "user@" or "user:password@" of a URL authority.
// A git remote is routinely written as https://<token>@host/repo, and the
// tools that fail to reach it quote the remote back at us.
var uriUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]+@`)

// ScrubURIUserinfo replaces the userinfo of every URL in s with "***".
// Anything a leg records — a backup_run error above all — goes through it,
// because a token echoed by a subprocess is a token in the database.
func ScrubURIUserinfo(s string) string {
	return uriUserinfo.ReplaceAllString(s, "$1***@")
}

// HasURIUserinfo reports whether s carries userinfo in a URL authority, so the
// CLI can object before such a uri is stored.
func HasURIUserinfo(s string) bool {
	return uriUserinfo.MatchString(s)
}

// scrubbed rewrites an error's text with every URL userinfo removed. The
// original is dropped rather than wrapped: errors.Unwrap on a wrapper would
// hand the unscrubbed text straight back.
func scrubbed(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	clean := ScrubURIUserinfo(text)
	if clean == text {
		return err
	}
	return errors.New(clean)
}

// gitLeg tars the staging tree, compresses it, encrypts it to every keyring
// recipient, and force-pushes it as a single orphan commit. keyring.age rides
// along unencrypted: it is the ciphertext that unwraps everything else.
func (e *Engine) gitLeg(ctx context.Context, t Target, stage string) (_ string, _ int64, err error) {
	defer func() { err = scrubbed(err) }()

	if err := checkRepoSize(ctx, t.URI); err != nil {
		return "", 0, err
	}

	recipients, err := ageRecipients(e.env.Recipients())
	if err != nil {
		return "", 0, err
	}

	archive, ext, err := archiveDir(ctx, stage)
	if err != nil {
		return "", 0, err
	}
	sealed, err := ageEncrypt(archive, recipients)
	if err != nil {
		return "", 0, err
	}

	keyring, err := os.ReadFile(filepath.Join(stage, KeyringName))
	if err != nil {
		return "", 0, fmt.Errorf("backup: read staged keyring: %w", err)
	}

	work, err := os.MkdirTemp(e.opts.StageRoot, "exocomp-git-")
	if err != nil {
		return "", 0, fmt.Errorf("backup: create git work dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	stamp := e.opts.Now().UTC().Format(stampLayout)
	snapshot := "snapshot-" + stamp + ext + ".age"
	parts, partsManifest := chunkSnapshot(snapshot, sealed)

	files := map[string][]byte{
		KeyringName:  keyring,
		RecoveryName: renderRecovery(t, e.env.Recipients(), e.opts.Now().UTC()),
	}
	maps.Copy(files, parts)
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(work, name), body, 0o644); err != nil {
			return "", 0, fmt.Errorf("backup: stage %s: %w", name, err)
		}
	}
	partsBody, err := writePartsManifest(work, partsManifest)
	if err != nil {
		return "", 0, err
	}
	// The parts manifest lives outside the age envelope, so on its own it is
	// the one place a chunked snapshot can be rewritten unnoticed.
	macKey := e.env.Derive(ManifestMACPurpose, t.Name)
	if len(macKey) != dekLen {
		return "", 0, errors.New("backup: envelope is closed")
	}
	defer clear(macKey)
	partsName := partsManifestName(snapshot)
	if err := WriteMACFile(filepath.Join(work, partsName+macSuffix), macKey, partsBody, t.Name, partsName); err != nil {
		return "", 0, err
	}

	addArgs := make([]string, 0, len(files)+2)
	for name := range files {
		addArgs = append(addArgs, name)
	}
	addArgs = append(addArgs, partsName, partsName+macSuffix)
	sort.Strings(addArgs)

	git := func(args ...string) (toolResult, error) {
		return e.runTool(ctx, executor.Spec{
			Kind: "shell",
			Argv: append([]string{"git"}, args...),
			Dir:  work,
			Env:  gitEnv(t.Env),
		}, nil)
	}
	steps := [][]string{
		{"init", "-q", "-b", "backup"},
		append([]string{"add", "--"}, addArgs...),
		{"-c", "user.name=exocomp", "-c", "user.email=exocomp@invalid",
			"commit", "-q", "--no-gpg-sign", "-m", "exocomp backup " + stamp},
		{"push", "--force", "--quiet", t.URI, "HEAD:" + backupRef},
	}
	for _, args := range steps {
		res, err := git(args...)
		if err != nil {
			return "", 0, err
		}
		if res.exitCode != 0 {
			return "", 0, fmt.Errorf("git %s exited %d: %s", args[0], res.exitCode, res.tail(5))
		}
	}

	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", 0, err
	}
	if head.exitCode != 0 || len(head.stdout) == 0 {
		return "", int64(len(sealed)), fmt.Errorf("git rev-parse exited %d", head.exitCode)
	}
	return strings.TrimSpace(head.stdout[len(head.stdout)-1]), int64(len(sealed)), nil
}

// gitEnv keeps git away from the operator's global config: a commit.gpgsign or
// a hook path inherited from the host would fail the leg for reasons that have
// nothing to do with the backup.
func gitEnv(extra map[string]string) []string {
	env := map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_TERMINAL_PROMPT": "0",
	}
	for k, v := range extra {
		env[k] = v
	}
	return toolEnv(env)
}

func ageRecipients(rs []secrets.Recipient) ([]age.Recipient, error) {
	if len(rs) == 0 {
		return nil, fmt.Errorf("backup: keyring has no recipients")
	}
	out := make([]age.Recipient, 0, len(rs))
	for _, r := range rs {
		parsed, err := age.ParseX25519Recipient(r.Public)
		if err != nil {
			return nil, fmt.Errorf("backup: recipient %s: %w", r.Label, err)
		}
		out = append(out, parsed)
	}
	return out, nil
}

// archiveDir tars the regular files in dir and compresses the result. It
// returns the archive and the extension describing it, so a restore can tell
// zstd from gzip by name as well as by magic bytes.
func archiveDir(ctx context.Context, dir string) ([]byte, string, error) {
	raw, err := tarDir(dir)
	if err != nil {
		return nil, "", err
	}
	if _, err := exec.LookPath("zstd"); err != nil {
		out, err := gzipBytes(raw)
		return out, ".tar.gz", err
	}
	out, err := filterBytes(ctx, raw, "zstd", "-q", "-c")
	if err != nil {
		return nil, "", err
	}
	return out, ".tar.zst", nil
}

func tarDir(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("backup: read staging dir: %w", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("backup: read %s: %w", e.Name(), err)
		}
		hdr := &tar.Header{Name: e.Name(), Mode: 0o600, Size: int64(len(body)), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("backup: tar %s: %w", e.Name(), err)
		}
		if _, err := tw.Write(body); err != nil {
			return nil, fmt.Errorf("backup: tar %s: %w", e.Name(), err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("backup: tar: %w", err)
	}
	return buf.Bytes(), nil
}

func untar(data []byte, dir string) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("backup: untar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Base(filepath.Clean(hdr.Name))
		if name == "." || name == string(filepath.Separator) {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
		if err != nil {
			return fmt.Errorf("backup: untar %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			return fmt.Errorf("backup: untar %s: %w", name, err)
		}
	}
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("backup: gzip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("backup: gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// decompress sniffs the archive's magic bytes rather than trusting its name.
func decompress(ctx context.Context, data []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		if _, err := exec.LookPath("zstd"); err != nil {
			return nil, fmt.Errorf("backup: archive is zstd-compressed and zstd is not on PATH")
		}
		return filterBytes(ctx, data, "zstd", "-d", "-q", "-c")
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("backup: gunzip: %w", err)
		}
		defer func() { _ = zr.Close() }()
		out, err := io.ReadAll(zr)
		if err != nil {
			return nil, fmt.Errorf("backup: gunzip: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("backup: archive is neither zstd nor gzip")
	}
}

// filterBytes pipes in through a tool and returns its stdout. The executor
// driver deliberately has no stdin, so a pure data filter runs directly;
// correctness is still the exit code.
func filterBytes(ctx context.Context, in []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = toolEnv(nil)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("backup: %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
