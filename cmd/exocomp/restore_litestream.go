package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/store"
)

type litestreamRestoreArgs struct {
	dest             string
	identity         string
	replica          string
	replicaEncrypted bool
}

// runLitestreamRestore recovers exocomp.db from the Litestream replica — the
// seconds-RPO complement to the restic chain, never a replacement for it. It
// recovers the DATABASE ONLY, onto a data dir that already holds the keyring, and
// refuses to trust the result until a decrypt round-trip proves the keyring opens
// it. It never resumes schedules: a restore leaves the system paused, matching
// the recovery posture of the full restic restore.
func runLitestreamRestore(cmd *cobra.Command, a litestreamRestoreArgs) error {
	out := cmd.OutOrStdout()
	warn := func(msg string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: "+msg) }

	if a.identity == "" {
		return errors.New("restore: --identity is required for --from-litestream " +
			"(the decrypt round-trip proves the recovered db before it is trusted)")
	}

	// The keyring must already be at the destination: Litestream ships the
	// database alone, and only the keyring can open its sealed rows. Its absence
	// means the authoritative restic restore has not run yet.
	keyringFile := filepath.Join(a.dest, backup.KeyringName)
	if _, err := os.Stat(keyringFile); err != nil {
		return fmt.Errorf("restore: %s holds no %s — run the full restic restore first, "+
			"then use --from-litestream to fast-forward the database: %w",
			a.dest, backup.KeyringName, err)
	}

	replica, err := resolveReplica(cmd, a.replica)
	if err != nil {
		return err
	}

	// Restore to a sidecar file and promote it only after the decrypt round-trip
	// passes, so a failed or forged restore never overwrites the live database.
	liveDB := filepath.Join(a.dest, backup.RestoredDBName)
	pending := liveDB + ".ls-restoring"
	_ = os.Remove(pending)

	err = backup.LitestreamRestore(cmd.Context(), backup.LitestreamConfig{
		ReplicaURL: replica,
		DestDB:     pending,
		Ack:        a.replicaEncrypted,
		Warn:       warn,
	})
	if errors.Is(err, backup.ErrLitestreamAbsent) {
		return fmt.Errorf("restore: the litestream sidecar is not installed on this host; "+
			"the fast-RPO path is unavailable — use the authoritative restic restore instead: %w", err)
	}
	if err != nil {
		_ = os.Remove(pending)
		return err
	}

	name, err := backup.VerifyDBDecrypts(cmd.Context(), pending, keyringFile, a.identity)
	if err != nil {
		// An unverified restore is not promoted: the live database stays as it was.
		_ = os.Remove(pending)
		return fmt.Errorf("restore: recovered database failed the decrypt round-trip and was discarded: %w", err)
	}

	if err := promoteRestoredDB(pending, liveDB); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "restored %s from litestream replica %s\n", liveDB, replica)
	_, _ = fmt.Fprintf(out, "  scope:    database only (keyring and secrets come from restic)\n")
	_, _ = fmt.Fprintf(out, "  verified: decrypt round-trip opened sealed row %q\n", name)
	_, _ = fmt.Fprintf(out, "  paused:   schedules are NOT resumed; start `exocomp serve` deliberately\n")
	return nil
}

// resolveReplica takes the replica url from the flag, or reads it from the
// stored setting the replicate command writes. It never invents one: an absent
// replica is an honest refusal.
func resolveReplica(cmd *cobra.Command, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	st, err := openStore()
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()

	url, err := st.GetSetting(cmd.Context(), backup.SettingReplicaURL)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("restore: no litestream replica configured — pass --replica, "+
			"or set %s", backup.SettingReplicaURL)
	}
	if err != nil {
		return "", err
	}
	return url, nil
}

// promoteRestoredDB renames the verified database over the live one and clears
// the WAL/SHM sidecars that belonged to the replaced file — left behind they
// would graft a stale log onto the restored database.
func promoteRestoredDB(pending, live string) error {
	if err := os.Rename(pending, live); err != nil {
		return fmt.Errorf("restore: promote recovered database: %w", err)
	}
	for _, sidecar := range []string{live + "-wal", live + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restore: clear %s: %w", filepath.Base(sidecar), err)
		}
	}
	return nil
}
