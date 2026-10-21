package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestRenderRecoveryImageRef: the recovery notes name the image this
// deployment runs, because a local tag dies with the host. An empty ref must
// omit the line entirely rather than print an empty value an operator could
// read as "nothing to load".
func TestRenderRecoveryImageRef(t *testing.T) {
	t.Parallel()

	tgt := Target{BackupTarget: store.BackupTarget{
		Name: "nas", Kind: KindResticLocal, URI: "/srv/exocomp",
	}}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	withFacts := factsBlock(t, string(renderRecovery(tgt, nil, now, "ghcr.io/agync/exocomp:latest")))
	t.Logf("Facts with a ref:\n%s", withFacts)
	if !strings.Contains(withFacts, "image-ref: ghcr.io/agync/exocomp:latest\n") {
		t.Errorf("Facts block does not carry the image ref:\n%s", withFacts)
	}

	without := string(renderRecovery(tgt, nil, now, ""))
	t.Logf("Facts without a ref:\n%s", factsBlock(t, without))
	if strings.Contains(without, "image-ref:") {
		t.Errorf("an empty ref still produced an image-ref line:\n%s", without)
	}
}

// factsBlock returns the text between the Facts fence, so an assertion cannot
// accidentally match an image-ref mention elsewhere in the notes.
func factsBlock(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "## Facts\n\n```\n")
	if !ok {
		t.Fatalf("recovery notes have no Facts block:\n%s", body)
	}
	facts, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatalf("Facts block is unterminated:\n%s", body)
	}
	return facts
}

// handCommands returns the lines of the "Recover by hand" fence.
func handCommands(t *testing.T, body string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "## Recover by hand\n\n```sh\n")
	if !ok {
		t.Fatalf("recovery notes have no Recover by hand fence:\n%s", body)
	}
	fence, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatalf("Recover by hand fence is unterminated:\n%s", body)
	}
	return strings.Split(strings.TrimRight(fence, "\n"), "\n")
}

// TestGitRecoveryGlobMatchesTheWrittenParts: the git leg writes every snapshot
// as <name>.age.partNN beside a .age.parts.json manifest and its .mac, never
// as a bare .age file. The card's part glob has to select exactly the parts,
// for the zstd and the gzip name alike and across the point where the part
// number gains a digit. It is serial because it shrinks chunkSize.
func TestGitRecoveryGlobMatchesTheWrittenParts(t *testing.T) {
	tgt := Target{BackupTarget: store.BackupTarget{Name: "offsite", Kind: KindGit, URI: "git@example.invalid:x/y.git"}}
	card := string(renderRecovery(tgt, nil, time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC), ""))

	var glob string
	for _, line := range handCommands(t, card) {
		if m := regexp.MustCompile(`snapshots/(snapshot-\S+)`).FindStringSubmatch(line); m != nil {
			glob = m[1]
		}
	}
	if glob == "" {
		t.Fatalf("the git card names no snapshot glob:\n%s", card)
	}

	old := chunkSize
	chunkSize = 4
	t.Cleanup(func() { chunkSize = old })

	for _, ext := range []string{".tar.zst", ".tar.gz"} {
		for _, parts := range []int{2, 101} {
			snapshot := "snapshot-20260301T020000Z" + ext + ".age"
			files, manifest := chunkSnapshot(snapshot, make([]byte, parts*int(chunkSize)))
			if len(manifest.Parts) != parts {
				t.Fatalf("wrote %d parts, want %d", len(manifest.Parts), parts)
			}
			cases := map[string]bool{
				partsManifestName(snapshot):             false,
				partsManifestName(snapshot) + macSuffix: false,
			}
			var names []string
			for name := range files {
				cases[name] = true
				names = append(names, name)
			}
			for name, want := range cases {
				got, err := filepath.Match(glob, name)
				if err != nil {
					t.Fatalf("glob %q: %v", glob, err)
				}
				if got != want {
					t.Errorf("glob %q matches %q = %v, want %v", glob, name, got, want)
				}
			}

			// The shell expands the glob in lexical order, which has to be the
			// order the parts were written in.
			sort.Strings(names)
			for i, entry := range manifest.Parts {
				if names[i] != entry.Name {
					t.Fatalf("lexical order puts %q at %d, the writer put %q there", names[i], i, entry.Name)
				}
			}
		}
	}
}

// TestGitRecoveryCardRecoversARealSnapshot runs the card's by-hand commands,
// as printed in the RECOVERY.md the leg commits, against a snapshot the leg
// just wrote in several parts. The card is meant to work with nothing of
// exocomp's on the host.
func TestGitRecoveryCardRecoversARealSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real git")
	}
	requireTool(t, "git")
	requireTool(t, "zstd")
	f := newFixture(t)

	old := chunkSize
	chunkSize = 256
	t.Cleanup(func() { chunkSize = old })

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "init", "--bare", "--quiet", bare)
	target := f.target(t, "offsite-git-card", KindGit, bare)
	report, err := f.engine.Run(t.Context(), []Target{target})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("git leg: %v", err)
	}

	card := gitRun(t, bare, "show", backupRef+":"+RecoveryName)
	script := strings.ReplaceAll(strings.Join(handCommands(t, card), "\n"), "/srv/exocomp", "restored")

	work := t.TempDir()
	key, err := os.ReadFile(f.identity)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "recovery.key"), key, 0o600); err != nil {
		t.Fatalf("stage identity: %v", err)
	}
	if err := os.Mkdir(filepath.Join(work, "restored"), 0o700); err != nil {
		t.Fatalf("create restore dir: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "sh", "-ec", script)
	cmd.Dir = work
	cmd.Env = gitEnv(nil)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the card's commands failed: %v\n%s\n%s", err, out, script)
	}
	for _, name := range []string{SnapshotName, KeyringName, ManifestName} {
		if _, err := os.Stat(filepath.Join(work, "restored", name)); err != nil {
			t.Errorf("the card did not restore %s: %v", name, err)
		}
	}
}
