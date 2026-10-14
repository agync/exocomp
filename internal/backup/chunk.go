package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// chunkSize bounds one part of a chunked git-leg snapshot. 90 MB keeps every
// part under the 100 MB single-file limit GitHub and most git hosts enforce
// (the chunking amendment). It is a var, not a const, so a test can
// shrink it instead of generating a genuinely huge fixture.
var chunkSize int64 = 90 * 1024 * 1024

// repoSizeGuard is the bare repository size, as repoDU sees the objects
// directory, past which the git leg refuses rather than growing a remote
// nobody sized for it (the chunking amendment).
const repoSizeGuard int64 = 4 * 1024 * 1024 * 1024

// partsSuffix names the manifest that lists one snapshot's parts, appended to
// the snapshot's own name (itself already unique per run).
const partsSuffix = ".parts.json"

// PartEntry is one chunk of a chunked, age-encrypted snapshot.
type PartEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PartsManifest lists every part of one chunked snapshot, in the order they
// concatenate back into the whole. Size and SHA256 describe the reassembled
// archive, so a restore catches a part that is individually intact but was
// swapped, dropped, or reordered.
type PartsManifest struct {
	Snapshot string      `json:"snapshot"`
	Size     int64       `json:"size"`
	SHA256   string      `json:"sha256"`
	Parts    []PartEntry `json:"parts"`
}

// partsManifestName is the file a chunked snapshot's manifest is written
// under, alongside its parts.
func partsManifestName(snapshot string) string {
	return snapshot + partsSuffix
}

// chunkSnapshot splits sealed into chunkSize-or-smaller parts named
// "<snapshot>.partNNN" and returns them alongside the manifest describing
// them. A sealed archive at or under chunkSize still gets exactly one part
// ("...part00"), so a restore has one reassembly path regardless of size.
func chunkSnapshot(snapshot string, sealed []byte) (map[string][]byte, PartsManifest) {
	whole := sha256.Sum256(sealed)
	manifest := PartsManifest{
		Snapshot: snapshot,
		Size:     int64(len(sealed)),
		SHA256:   hex.EncodeToString(whole[:]),
	}

	n := len(sealed)/int(chunkSize) + 1
	if len(sealed) > 0 && len(sealed)%int(chunkSize) == 0 {
		n--
	}
	digits := 2
	if got := len(strconv.Itoa(n - 1)); got > digits {
		digits = got
	}

	files := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		start := i * int(chunkSize)
		end := min(start+int(chunkSize), len(sealed))
		body := sealed[start:end]
		sum := sha256.Sum256(body)
		name := fmt.Sprintf("%s.part%0*d", snapshot, digits, i)
		files[name] = body
		manifest.Parts = append(manifest.Parts, PartEntry{
			Name:   name,
			Size:   int64(len(body)),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}
	return files, manifest
}

// reassembleSnapshot concatenates a chunked snapshot's parts from dir,
// verifying each part's own sha256 before it joins the whole, then verifying
// the reassembled archive's sha256 and size against the manifest. A part that
// is missing, truncated, reordered, or individually tampered fails here
// rather than handing decrypt() a silently wrong archive.
func reassembleSnapshot(dir string, manifest PartsManifest) ([]byte, error) {
	if len(manifest.Parts) == 0 {
		return nil, fmt.Errorf("backup: parts manifest for %s lists no parts", manifest.Snapshot)
	}
	buf := make([]byte, 0, manifest.Size)
	for _, part := range manifest.Parts {
		body, err := os.ReadFile(filepath.Join(dir, part.Name))
		if err != nil {
			return nil, fmt.Errorf("backup: read part %s: %w", part.Name, err)
		}
		if int64(len(body)) != part.Size {
			return nil, fmt.Errorf("backup: part %s is %d bytes, manifest says %d", part.Name, len(body), part.Size)
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != part.SHA256 {
			return nil, fmt.Errorf("backup: part %s sha256 %s, manifest says %s", part.Name, got, part.SHA256)
		}
		buf = append(buf, body...)
	}
	if int64(len(buf)) != manifest.Size {
		return nil, fmt.Errorf("backup: reassembled %s is %d bytes, manifest says %d", manifest.Snapshot, len(buf), manifest.Size)
	}
	sum := sha256.Sum256(buf)
	if got := hex.EncodeToString(sum[:]); got != manifest.SHA256 {
		return nil, fmt.Errorf("backup: reassembled %s sha256 %s, manifest says %s", manifest.Snapshot, got, manifest.SHA256)
	}
	return buf, nil
}

// writePartsManifest writes the manifest and returns the exact bytes it wrote,
// which are the bytes the parts tag authenticates.
func writePartsManifest(dir string, manifest PartsManifest) ([]byte, error) {
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("backup: marshal parts manifest for %s: %w", manifest.Snapshot, err)
	}
	body = append(body, '\n')
	name := partsManifestName(manifest.Snapshot)
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		return nil, fmt.Errorf("backup: write %s: %w", name, err)
	}
	return body, nil
}

func readPartsManifest(dir, snapshot string) (PartsManifest, []byte, error) {
	name := partsManifestName(snapshot)
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return PartsManifest{}, nil, fmt.Errorf("backup: read %s: %w", name, err)
	}
	var m PartsManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return PartsManifest{}, nil, fmt.Errorf("backup: parse %s: %w", name, err)
	}
	return m, body, nil
}

// repoDU reports the size in bytes of a bare git repository's objects
// directory, the way `du` sees it. It is a var so a test can fake a size no
// fixture could actually grow a repository to; checkRepoSize is the only
// caller.
var repoDU = func(ctx context.Context, path string) (int64, error) {
	objects := filepath.Join(path, "objects")
	out, err := exec.CommandContext(ctx, "du", "-sk", objects).Output()
	if err != nil {
		return 0, fmt.Errorf("backup: du %s: %w", objects, err)
	}
	field, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if field == "" {
		field, _, _ = strings.Cut(strings.TrimSpace(string(out)), " ")
	}
	kb, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("backup: du %s: parse %q: %w", objects, string(out), err)
	}
	return kb * 1024, nil
}

// checkRepoSize refuses a git leg whose bare repository has already grown
// past repoSizeGuard. It only judges a repository it can measure: a target
// that does not exist yet, or is not a local path `du` can walk, is not a
// size problem — the push itself reports that failure.
func checkRepoSize(ctx context.Context, path string) error {
	if _, err := os.Stat(filepath.Join(path, "objects")); err != nil {
		return nil
	}
	size, err := repoDU(ctx, path)
	if err != nil {
		return nil
	}
	if size > repoSizeGuard {
		return fmt.Errorf("backup: bare repo %s objects are %d bytes, over the %d byte guard",
			path, size, repoSizeGuard)
	}
	return nil
}
