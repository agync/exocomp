package wire

import "exocomp.dev/exocomp/internal/store"

// Runner payload kinds.
const (
	KindRunnerList = "runner.list"
	KindRunner     = "runner.get"
)

// RunnerCapsView projects the resource ceilings a runner declares. They are the
// design §6 caps the deferred container tier would enforce; nothing executes
// them today.
type RunnerCapsView struct {
	MemoryMax string `json:"memory_max,omitempty"`
	CPUQuota  string `json:"cpu_quota,omitempty"`
}

// RunnerView projects a registered runner definition.
//
// The bootstrap body is structurally absent: there is no field it could occupy,
// in the list or the single-object projection. It is not secret material — a
// recipe holds none (invariant 4) — but it is an arbitrarily long script, and a
// projection that inlined it would blow the envelope's low-token contract for
// every listing. Its digest and line count identify and size it; `exocomp
// runner show` prints the body itself on the text path, which is the human
// surface for reading a script.
type RunnerView struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Mode string `json:"mode"`
	Base string `json:"base"`
	// BootstrapSHA256 and BootstrapLines stand in for the Model A script body.
	BootstrapSHA256 string `json:"bootstrap_sha256,omitempty"`
	BootstrapLines  int    `json:"bootstrap_lines,omitempty"`
	// Containerfile is the Model B path — a reference, not the file's content.
	Containerfile string         `json:"containerfile,omitempty"`
	Packages      []string       `json:"packages,omitempty"`
	AnsibleCore   string         `json:"ansible_core,omitempty"`
	Caps          RunnerCapsView `json:"caps"`
	ContentHash   string         `json:"content_hash"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
}

// NewRunnerView projects one runner.
func NewRunnerView(r store.Runner) RunnerView {
	return RunnerView{
		ID:              r.ID,
		Name:            r.Name,
		Mode:            r.Mode,
		Base:            r.Base,
		BootstrapSHA256: r.BootstrapSHA256(),
		BootstrapLines:  r.BootstrapLines(),
		Containerfile:   r.Containerfile,
		Packages:        r.Packages,
		AnsibleCore:     r.AnsibleCore,
		Caps:            RunnerCapsView{MemoryMax: r.MemoryMax, CPUQuota: r.CPUQuota},
		ContentHash:     r.ContentHash,
		CreatedAt:       rfc3339(r.CreatedAt),
		UpdatedAt:       rfc3339(r.UpdatedAt),
	}
}

// NewRunnerList projects runners in store order (by name).
func NewRunnerList(rows []store.Runner) []RunnerView {
	out := make([]RunnerView, 0, len(rows))
	for _, r := range rows {
		out = append(out, NewRunnerView(r))
	}
	return out
}
