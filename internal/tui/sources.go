package tui

// sourceRow is one run-tree source (ADR 0010) on the sources surface: where a
// run's tree comes from, the ref it is pinned to, the commit its last sync
// landed on, and its trust posture. Read-only — registration stays a
// CLI-and-local-auth path, so this screen carries no mutation.
type sourceRow struct {
	name    string // "core-infra"
	kind    string // git|hostpath|volpath
	ref     string // "refs/heads/main"
	sync    string // short last-sync sha
	trust   string // "SIGNED · SIGNERS", "SIGNED", "unsigned"
	enabled string // "yes"/"no"
}

// SourcesModel is the sources screen at 120 columns.
type SourcesModel struct {
	summary string // "sources · 3 registered"
	clock   string
	rows    []sourceRow
}

const sourcesKeys = "read-only · register with exocomp source add"

// renderSourcesScreen draws the sources table. The trust column carries the
// signed/allowed-signers posture as a label, never color-only, so an unsigned
// tree reads as plainly as a pinned one.
func renderSourcesScreen(m SourcesModel) string {
	const w = 120
	lines := []string{
		lr(m.summary, m.clock, w),
		hrule(w),
		row(cell{1, "SOURCE"}, cell{20, "KIND"}, cell{31, "REF"}, cell{52, "LAST SYNC"},
			cell{68, "TRUST"}, cell{104, "ENABLED"}),
	}
	if len(m.rows) == 0 {
		lines = append(lines, "", " no sources registered — add one with exocomp source add")
	}
	for _, r := range m.rows {
		lines = append(lines, row(
			cell{1, r.name}, cell{20, r.kind}, cell{31, r.ref}, cell{52, r.sync},
			cell{68, r.trust}, cell{104, r.enabled},
		))
	}
	lines = append(lines, "", lr(sourcesKeys, "[0-9] screens", w))
	return join(lines)
}
