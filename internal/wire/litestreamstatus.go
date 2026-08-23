package wire

// KindLitestreamStatus is the kind of a litestream sidecar status report.
const KindLitestreamStatus = "litestream.status"

// The litestream replication states. Each mirrors one branch of the text
// report (cmd/exocomp/litestream.go's printLitestreamStatusText), so an agent
// gets the identical distinction the human report already draws.
const (
	LitestreamReplicationFresh         = "fresh"          // a replicated frame exists; ReplicaFreshAt is set
	LitestreamReplicationNoFrames      = "no_frames"      // local target configured, sidecar not running yet
	LitestreamReplicationRemoteUnknown = "remote_unknown" // remote target; age unobservable without the binary
	LitestreamReplicationProbeFailed   = "probe_failed"   // freshness probe errored
	LitestreamReplicationUnparseable   = "unparseable"    // the persisted target URI does not parse
	LitestreamReplicationNotConfigured = "not_configured" // no replica target set yet
	LitestreamReplicationUnknown       = "unknown"        // store/settings read failed
)

// LitestreamStatusView is the WAL-replication sidecar's local status: binary
// presence, whether a replica target is configured and its type, and whether
// replication is caught up.
//
// The target's raw URI is deliberately never emitted: an sftp:// target can
// carry an embedded user:pass, so only its type travels (the same omission
// internal/health's litestreamEntry already makes for the doctor summary).
// ReplicaFreshAt is an absolute RFC3339 timestamp, not a relative age, so
// repeated polls of an unchanged replica produce byte-identical output.
type LitestreamStatusView struct {
	BinaryPresent    bool   `json:"binary_present"`
	BinaryVersion    string `json:"binary_version,omitempty"`
	TargetConfigured bool   `json:"target_configured"`
	TargetType       string `json:"target_type,omitempty"`
	TargetRemote     bool   `json:"target_remote,omitempty"`
	ReplicationState string `json:"replication_state"`
	ReplicaFreshAt   string `json:"replica_fresh_at,omitempty"`
}
