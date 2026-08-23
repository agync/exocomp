package wire

import "exocomp.dev/exocomp/internal/store"

// CLI allow-list payload kinds.
const (
	KindCLIToolList = "cli-tool.list"
	KindCLITool     = "cli-tool.get"
)

// CLIToolView projects one allow-list entry: which binary may run, what is
// pinned onto every invocation of it, and what it may reach. None of it is
// secret — an allow-list entry holds no credential (invariant 4), only the
// boundary itself.
type CLIToolView struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Binary     string   `json:"binary"`
	FixedArgs  []string `json:"fixed_args,omitempty"`
	ArgPattern string   `json:"arg_pattern,omitempty"`
	// Egress is "none" or "https": the run's whole outbound network policy.
	Egress      string `json:"egress"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// NewCLIToolView projects one registered tool.
func NewCLIToolView(t store.CLITool) CLIToolView {
	return CLIToolView{
		ID:          t.ID,
		Name:        t.Name,
		Binary:      t.Binary,
		FixedArgs:   t.FixedArgs,
		ArgPattern:  t.ArgPattern,
		Egress:      t.Egress,
		Description: t.Description,
		CreatedAt:   rfc3339(t.CreatedAt),
		UpdatedAt:   rfc3339(t.UpdatedAt),
	}
}

// NewCLIToolList projects the allow-list in store order (by name).
func NewCLIToolList(rows []store.CLITool) []CLIToolView {
	out := make([]CLIToolView, 0, len(rows))
	for _, t := range rows {
		out = append(out, NewCLIToolView(t))
	}
	return out
}
