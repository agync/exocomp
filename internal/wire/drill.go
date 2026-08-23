package wire

import "exocomp.dev/exocomp/internal/backup"

// KindBackupDrill names a restore drill's report: `exocomp backup drill
// --output json`.
const KindBackupDrill = "backup.drill"

// DrillView projects one target's restore drill outcome. It carries no
// secret material — no password, no restic env, no envelope key — only what
// the drill found, so it is safe to print or log whole.
type DrillView struct {
	Target        string `json:"target"`
	Source        string `json:"source"`
	SnapshotID    string `json:"snapshot_id,omitempty"`
	RPOSeconds    int64  `json:"rpo_seconds,omitempty"`
	OK            bool   `json:"ok"`
	Reason        string `json:"reason,omitempty"`
	VerifiedFiles int    `json:"verified_files,omitempty"`
	VerifiedBytes int64  `json:"verified_bytes,omitempty"`
	// Error carries an infra failure — the drill itself could not run — as
	// distinct from Reason, which is a drill finding: the drill ran and the
	// backup it inspected did not check out.
	Error string `json:"error,omitempty"`
}

// NewDrillView projects one target's drill result. target is the store
// target name; result.Source already carries "<kind>:<uri>" or
// "litestream:<replica>".
func NewDrillView(target string, result backup.DrillResult) DrillView {
	return DrillView{
		Target:        target,
		Source:        result.Source,
		SnapshotID:    result.SnapshotID,
		RPOSeconds:    int64(result.RPOAge.Seconds()),
		OK:            result.OK,
		Reason:        result.Reason,
		VerifiedFiles: result.VerifiedFiles,
		VerifiedBytes: result.VerifiedBytes,
	}
}

// NewDrillErrorView projects a target the drill could not even run against —
// an infra error, not a finding (internal/backup/drill.go's own
// distinction).
func NewDrillErrorView(target, source string, err error) DrillView {
	return DrillView{Target: target, Source: source, Error: err.Error()}
}
