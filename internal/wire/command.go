package wire

import "exocomp.dev/exocomp/internal/executor"

// KindCommandResult is the wire kind for structured ad-hoc command output.
const KindCommandResult = "command.result"

// CommandResultView is the structured per-host execution summary returned by
// `exocomp command --output json`.
type CommandResultView struct {
	JobID    int64                                 `json:"job_id"`
	State    string                                `json:"state"`
	ExitCode int                                   `json:"exit_code"`
	Hosts    map[string]executor.HostCommandResult `json:"hosts"`
}

// NewCommandResultView constructs a CommandResultView from a finished job and parsed host results.
func NewCommandResultView(jobID int64, state string, exitCode int, hosts map[string]executor.HostCommandResult) CommandResultView {
	if hosts == nil {
		hosts = make(map[string]executor.HostCommandResult)
	}
	return CommandResultView{
		JobID:    jobID,
		State:    state,
		ExitCode: exitCode,
		Hosts:    hosts,
	}
}
