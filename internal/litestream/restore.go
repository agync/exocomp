package litestream

import "errors"

// RestoreArgs builds the deterministic `litestream restore` argument vector that
// reconstructs dbPath from its replica. It does not run litestream — it returns
// the exact argv so the caller can execute it (when the binary is present) and a
// test can pin it (when it is not). This is the Litestream fast-path restore; it
// complements, and never replaces, the restic restore the DR runbook drives.
//
// The replica is addressed through the generated config so a single source of
// truth (the DB path + target) drives both replication and restore, rather than
// re-deriving the replica URL at restore time. -if-replica-exists makes a
// restore into an already-populated data dir a no-op instead of an error, which
// is what a "catch up from the replica" call wants.
func RestoreArgs(configPath, dbPath string) ([]string, error) {
	if configPath == "" {
		return nil, errors.New("litestream: empty config path")
	}
	if dbPath == "" {
		return nil, errors.New("litestream: empty db path")
	}
	return []string{
		"restore",
		"-config", configPath,
		"-if-replica-exists",
		"-o", dbPath,
		dbPath,
	}, nil
}
