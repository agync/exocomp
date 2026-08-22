// Package litestream generates and reasons about the optional Litestream
// sidecar that replicates exocomp.db's WAL for seconds-level RPO (ADR 0012).
//
// The sidecar runs BESIDE exocompd, never inside it: this package emits a
// litestream.yml config and observes a replica's freshness, but it never
// links, compiles in, or spawns litestream from within the daemon. That keeps
// the thin-core invariant (ADR 0001) intact — Litestream is a pinned tool the
// operator runs, like restic, not a second service the daemon owns.
//
// Litestream is the fast-RPO complement to the authoritative restic DR chain,
// not a replacement: restic stays the encrypted, drill-covered chain a bare
// host recovers from. Litestream does not encrypt WAL frames, so a replica
// target must be LAN-local or an already-encrypted store; a plaintext remote
// target is refused unless the operator explicitly acknowledges the caveat.
//
// Everything here is deterministic and unit-testable without the litestream
// binary present: config generation is pure string rendering, and freshness is
// read from the replica's own filesystem layout.
package litestream

// Binary is the sidecar executable exocomp expects on PATH. It is absent on a
// fresh host by design (optional sidecar), so every caller degrades honestly
// rather than assuming it is installed.
const Binary = "litestream"

// InstallHint names the one step that makes the sidecar usable, quoted verbatim
// when a command has to refuse for want of the binary.
const InstallHint = "install litestream (https://litestream.io/install) and re-run"

// Setting keys for the persisted replica target. Non-secret operator config, so
// it lives in the store's setting table; a target's credentials never do —
// those resolve from the sealed secret plane at sidecar launch.
const (
	// SettingReplicaTarget is the replica target URI (a path, file://, or s3://…).
	SettingReplicaTarget = "litestream.replica_target"
	// SettingReplicaAck records that the operator acknowledged replicating to a
	// remote target Litestream does not encrypt.
	SettingReplicaAck = "litestream.replica_unencrypted_ack"
)
