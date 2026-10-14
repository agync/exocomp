package tui

// secretRow is one sealed secret on the secrets surface, as the screen holds
// it: metadata and lifecycle only. The value, nonce and ciphertext are never
// carried here — invariant 4 (secrets never appear in logs, events, docs or
// fixtures) means the sealed material must not reach the view at all. source is
// how it was minted (generated|user); expiry is the staleness marker
// ("EXPIRED" | "expires in Nd" | "no expiry") computed against now in the live
// assembly; bindings names which run-secret bindings inject it and as which env
// var, or "unbound".
type secretRow struct {
	name     string // "restic-password"
	kind     string // the --kind label
	source   string // "generated" | "user"
	created  string // "2026-08-20"
	rotated  string // "2026-08-21" or "never"
	lastUsed string // "2026-08-22" or "never"
	expiry   string // "EXPIRED" | "expires in 5d" | "no expiry"
	bindings string // "used by: gitops as $DEPLOY_TOKEN" | "unbound"
}

// SecretsModel is the secrets screen at 120 columns.
type SecretsModel struct {
	summary string // "secrets · 3 stored"
	clock   string
	rows    []secretRow
}

const secretsKeys = "read-only · manage via exocomp secrets add|replace|rm"

// renderSecretsScreen draws the secrets table. Each secret is a metadata row
// followed by an indented bindings line, so the (binding -> env var) breakdown
// reads under the secret it injects. Source and the expiry staleness marker are
// labels, never color-only: an EXPIRED secret reads as plainly as a fresh one.
// No column carries the sealed value.
func renderSecretsScreen(m SecretsModel) string {
	const w = 120
	lines := []string{
		lr(m.summary, m.clock, w),
		hrule(w),
		row(cell{1, "NAME"}, cell{24, "KIND"}, cell{40, "SOURCE"}, cell{52, "CREATED"},
			cell{66, "CHANGED"}, cell{80, "LAST USED"}, cell{94, "EXPIRY"}),
	}
	if len(m.rows) == 0 {
		lines = append(lines, "", " no secrets stored — add one with exocomp secrets add")
	}
	for _, r := range m.rows {
		lines = append(lines, row(
			cell{1, r.name}, cell{24, r.kind}, cell{40, r.source}, cell{52, r.created},
			cell{66, r.rotated}, cell{80, r.lastUsed}, cell{94, r.expiry},
		))
		lines = append(lines, row(cell{3, r.bindings}))
	}
	lines = append(lines, "", lr(secretsKeys, "[0-9]/[s] screens", w))
	return join(lines)
}
