package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// DrillResult is a restore drill's structured report: whether the newest
// available backup would actually save you, not just whether it exists. OK
// is false only when a backup was reached and inspected but did not check
// out — a bad manifest digest, a tampered tag, a downgraded keyring, or a
// database that will not open or fails its integrity check. Anything that
// stops the drill before it reaches a snapshot to inspect (no backup was
// ever taken, the target is unreachable, the throwaway directory could not
// be created) comes back as a plain error instead: the drill itself failed
// to run, which is not the same finding as a bad backup.
type DrillResult struct {
	// Source names what was drilled: "<kind>:<uri>" for restic/git targets,
	// "litestream:<replica>" for the fast-RPO path.
	Source string
	// SnapshotID identifies the point in time recovered. It is the
	// manifest's own creation timestamp (RFC3339, UTC) rather than a
	// storage-layer id: the RPO figures in
	// docs/operations/disaster-recovery.md are measured against when
	// exocomp staged the snapshot, and every source (restic, git,
	// litestream) has that timestamp even though only some of them also
	// have a storage-native snapshot hash. Empty when OK is false.
	SnapshotID string
	// RPOAge is how long ago the drilled snapshot was taken. Zero when OK
	// is false.
	RPOAge time.Duration
	// OK is true when the snapshot verified and its database opened clean.
	OK bool
	// Reason explains a false OK. Empty when OK is true.
	Reason string
	// VerifiedFiles and VerifiedBytes count what Manifest.Verify checked.
	// Zero when OK is false.
	VerifiedFiles int
	VerifiedBytes int64
}

// String renders a one-line summary. It exists so a future caller — doctor,
// self-observability — can render a drill's outcome without depending on
// its fields; nothing calls it yet.
func (r DrillResult) String() string {
	if r.OK {
		return fmt.Sprintf("restore drill OK: %s snapshot %s, RPO %s, %d file(s) verified (%d bytes)",
			r.Source, r.SnapshotID, r.RPOAge.Round(time.Second), r.VerifiedFiles, r.VerifiedBytes)
	}
	return fmt.Sprintf("restore drill FAILED: %s: %s", r.Source, r.Reason)
}

// drillEventKey namespaces the drill's structured outcome inside a job
// event's raw_json.
const drillEventKey = "backup.drill"

// DrillEvent is the structured outcome the drill job attaches to its event's
// raw_json — the machine-readable twin of the human line DrillResult.String
// renders, carrying public data only: the source is scrubbed of embedded
// userinfo at the emit site (drill_job.go drillOutcomeRaw), and the queue's
// redactor scrubs the payload again before it is stored.
type DrillEvent struct {
	Source        string `json:"source"`
	SnapshotID    string `json:"snapshot,omitempty"`
	RPOAgeSeconds int64  `json:"rpo_age"`
	OK            bool   `json:"ok"`
	Reason        string `json:"reason,omitempty"`
	VerifiedFiles int    `json:"verified_files"`
	VerifiedBytes int64  `json:"verified_bytes"`
}

// DrillOutcomeFor reads a drill outcome back out of a job event's raw_json.
// ok is false when the event carries no drill payload.
func DrillOutcomeFor(rawJSON string) (outcome DrillEvent, ok bool) {
	if rawJSON == "" {
		return DrillEvent{}, false
	}
	var envelope struct {
		Drill *DrillEvent `json:"backup.drill"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &envelope); err != nil || envelope.Drill == nil {
		return DrillEvent{}, false
	}
	return *envelope.Drill, true
}

// RestoreDrill restores src's newest snapshot into a throwaway directory —
// os.MkdirTemp under stageRoot, mode 0700, always removed on return — runs
// the same Manifest.Verify a real restore runs (via RestoreLatest), and then
// opens the restored database and runs PRAGMA integrity_check to prove the
// backup is sound, not merely present. It never writes into or reads from a
// live data dir and never promotes anything: src names only the backup
// target, and the drill's one destination is the directory it creates and
// destroys itself.
//
// A verify or open failure comes back as DrillResult{OK: false, Reason:
// ...} rather than an error: the drill succeeded at its job, which was
// finding out whether the backup is good. An error return means the drill
// itself could not run.
func RestoreDrill(ctx context.Context, src Source, stageRoot string) (DrillResult, error) {
	drillDir, err := os.MkdirTemp(stageRoot, "exocomp-drill-")
	if err != nil {
		return DrillResult{}, fmt.Errorf("backup: restore drill: create throwaway dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(drillDir) }()
	if err := os.Chmod(drillDir, 0o700); err != nil {
		return DrillResult{}, fmt.Errorf("backup: restore drill: throwaway dir mode: %w", err)
	}

	if src.StageRoot == "" {
		src.StageRoot = stageRoot
	}

	result := DrillResult{Source: src.Kind + ":" + src.URI}

	manifest, err := RestoreLatest(ctx, src, drillDir)
	if err != nil {
		if isDrillFinding(err) {
			result.Reason = err.Error()
			return result, nil
		}
		return DrillResult{}, err
	}

	result.SnapshotID = manifest.CreatedAt.UTC().Format(time.RFC3339)
	result.RPOAge = time.Since(manifest.CreatedAt)
	result.VerifiedFiles = len(manifest.Files)
	for _, f := range manifest.Files {
		result.VerifiedBytes += f.Size
	}

	ok, reason, err := openAndCheck(ctx, filepath.Join(drillDir, RestoredDBName))
	if err != nil {
		return DrillResult{}, fmt.Errorf("backup: restore drill: %w", err)
	}
	result.OK = ok
	result.Reason = reason
	return result, nil
}

// openAndCheck opens the restored database — which runs the store's normal
// migrate-on-open — and then runs an explicit PRAGMA integrity_check over
// it: the "verify a freshly restored file before promoting" discipline
// litestream_restore.go documents, applied here to every drilled source, not
// only the fast-RPO one. A failure to open or a non-"ok" integrity check is
// the drill finding a bad backup, not the drill failing to run, so it comes
// back as (false, reason, nil) rather than an error.
func openAndCheck(ctx context.Context, dbPath string) (ok bool, reason string, err error) {
	st, openErr := store.Open(dbPath)
	if openErr != nil {
		return false, fmt.Sprintf("restored database will not open: %v", openErr), nil
	}
	defer func() { _ = st.Close() }()

	check, pragmaErr := st.Pragma(ctx, "integrity_check")
	if pragmaErr != nil {
		return false, fmt.Sprintf("restored database integrity check failed: %v", pragmaErr), nil
	}
	if check != "ok" {
		return false, fmt.Sprintf("restored database failed PRAGMA integrity_check: %s", check), nil
	}
	return true, "", nil
}

// isDrillFinding reports whether err is the drill doing its job — a
// snapshot that was reached but did not check out — rather than the drill
// failing to reach a snapshot at all.
func isDrillFinding(err error) bool {
	return errors.Is(err, ErrSnapshotCorrupt) ||
		errors.Is(err, ErrManifestTampered) ||
		errors.Is(err, ErrManifestUnauthenticated) ||
		errors.Is(err, ErrKeyringDowngrade)
}

// RestoreDrillLitestream drills the fast-RPO Litestream replica: it restores
// exocomp.db (database only, the one thing Litestream ships) into a
// throwaway directory holding a read-only COPY of liveDataDir's keyring —
// the live keyring is only ever read, never written to or moved — and runs
// the same decrypt round-trip (VerifyEnvelopeOpens) a real --from-litestream
// restore must pass before it would promote the file. Like RestoreDrill, the
// throwaway directory is discarded whatever the outcome, and nothing here
// ever writes into liveDataDir.
//
// RPOAge and VerifiedFiles/VerifiedBytes are left zero: a Litestream replica
// carries no manifest, so the drill has no staged-at timestamp or digest
// list to report — only the decrypt round-trip's pass/fail.
func RestoreDrillLitestream(ctx context.Context, cfg LitestreamConfig, liveDataDir, identityFile, stageRoot string) (DrillResult, error) {
	if !LitestreamAvailable() {
		return DrillResult{}, ErrLitestreamAbsent
	}

	drillDir, err := os.MkdirTemp(stageRoot, "exocomp-drill-litestream-")
	if err != nil {
		return DrillResult{}, fmt.Errorf("backup: restore drill: create throwaway dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(drillDir) }()
	if err := os.Chmod(drillDir, 0o700); err != nil {
		return DrillResult{}, fmt.Errorf("backup: restore drill: throwaway dir mode: %w", err)
	}

	for _, name := range []string{KeyringName, KeyringVersionName} {
		if err := copyReadOnly(filepath.Join(liveDataDir, name), filepath.Join(drillDir, name)); err != nil {
			return DrillResult{}, fmt.Errorf("backup: restore drill: stage keyring: %w", err)
		}
	}

	cfg.DestDB = filepath.Join(drillDir, RestoredDBName)
	result := DrillResult{Source: "litestream:" + cfg.ReplicaURL}

	if err := LitestreamRestore(ctx, cfg); err != nil {
		return DrillResult{}, err
	}

	if _, err := VerifyEnvelopeOpens(ctx, drillDir, identityFile); err != nil {
		result.Reason = fmt.Sprintf("decrypt round-trip failed: %v", err)
		return result, nil
	}
	result.OK = true
	return result, nil
}

// copyReadOnly copies src's bytes to dst. It never opens src for writing, so
// the caller's own copy — e.g. a live keyring — cannot be mutated by it.
func copyReadOnly(src, dst string) error {
	body, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("backup: read %s: %w", filepath.Base(src), err)
	}
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		return fmt.Errorf("backup: write %s: %w", filepath.Base(dst), err)
	}
	return nil
}
