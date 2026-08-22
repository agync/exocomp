package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// LocalSnapshot writes a self-contained, restorable copy of the store's durable
// state into dir: a vacuumed database plus a manifest that digests it. It reuses
// the VacuumInto + manifest primitives the fan-out engine stages with, but
// writes to a plain local directory instead of a remote target — so a
// self-upgrade can take a rollback point without a configured backup target on
// the box. dir is created 0700 (it will sit inside the 0700 data dir);
// VacuumInto refuses to overwrite an existing database, so a re-run over a
// populated dir errors rather than blessing a half-written snapshot.
func LocalSnapshot(ctx context.Context, st *store.Store, dir string) (Manifest, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("backup: create snapshot dir: %w", err)
	}
	if err := st.VacuumInto(ctx, filepath.Join(dir, SnapshotName)); err != nil {
		return Manifest{}, err
	}
	schema, err := st.SchemaVersion(ctx)
	if err != nil {
		return Manifest{}, err
	}
	return WriteManifest(dir, Manifest{
		SchemaVersion: schema,
		CreatedAt:     time.Now().UTC(),
		ToolVersions:  toolVersions(),
	})
}

// LocalRestore copies a LocalSnapshot's database back over the live one under
// dataDir, after re-verifying every digest in its manifest — a snapshot that
// does not verify is not restored (Manifest.Verify): a truncated database that
// opens is worse than a restore that stops. The caller MUST have closed the
// store first: the live file is replaced wholesale and its WAL/SHM sidecars
// removed, which an open handle would fight.
func LocalRestore(snapshotDir, dataDir string) error {
	m, err := ReadManifest(snapshotDir)
	if err != nil {
		return err
	}
	if err := m.Verify(snapshotDir); err != nil {
		return err
	}

	src := filepath.Join(snapshotDir, SnapshotName)
	dst := filepath.Join(dataDir, RestoredDBName)

	// Write the restored database beside the live one and rename it into place:
	// a crash mid-copy then leaves either the old file or the whole new one,
	// never half of either.
	tmp := dst + ".restoring"
	if err := copySnapshotFile(src, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("backup: restore database: %w", err)
	}

	// The WAL and shared-memory sidecars belong to the database just replaced;
	// left behind they would graft a stale log onto the restored file.
	for _, sidecar := range []string{dst + "-wal", dst + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("backup: clear %s: %w", filepath.Base(sidecar), err)
		}
	}
	return nil
}

func copySnapshotFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("backup: open snapshot db: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("backup: open restore target: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("backup: copy snapshot db: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("backup: flush restore target: %w", err)
	}
	return nil
}
