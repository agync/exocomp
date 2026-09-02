package tui

// runnerRow is one registered runner definition on the runners surface: the
// recipe metadata an operator places a fleet by. The bootstrap body and
// Containerfile path never reach this screen — a script is long and Mode
// already says which build model a runner uses, so the row carries only the
// columns a fixture and a glance can hold: Name, Mode, a sanely truncated
// Base, the package count, the ansible-core pin, a content-hash prefix, and
// Created.
type runnerRow struct {
	name    string // "fedora-ee"
	mode    string // "cached" | "onthefly"
	base    string // truncated base image reference
	pkgs    string // package count, "0" when none baked in
	core    string // ansible-core pin, "-" when unset
	hash    string // 12-char content-hash prefix
	created string // "2026-05-02"
}

// RunnersModel is the runners screen at 120 columns.
type RunnersModel struct {
	summary string // "runners · 2 registered"
	clock   string
	rows    []runnerRow
}

const runnersKeys = "read-only · manage via exocomp runner add|rm"

// renderRunnersScreen draws the runner registry table. It is single-row per
// definition — unlike Secrets' bindings line, a runner has no per-row
// breakdown that earns a second line, so the surface stays one row per
// runner. No bootstrap script or Containerfile body is ever a column.
func renderRunnersScreen(m RunnersModel) string {
	const w = 120
	lines := []string{
		lr(m.summary, m.clock, w),
		hrule(w),
		row(cell{1, "NAME"}, cell{22, "MODE"}, cell{33, "BASE"}, cell{69, "PKGS"},
			cell{75, "CORE"}, cell{85, "HASH"}, cell{99, "CREATED"}),
	}
	if len(m.rows) == 0 {
		lines = append(lines, "", " no runners registered — add one with exocomp runner add")
	}
	for _, r := range m.rows {
		lines = append(lines, row(
			cell{1, r.name}, cell{22, r.mode}, cell{33, r.base}, cell{69, r.pkgs},
			cell{75, r.core}, cell{85, r.hash}, cell{99, r.created},
		))
	}
	lines = append(lines, "", lr(runnersKeys, "[0-9]/[u] screens", w))
	return join(lines)
}
