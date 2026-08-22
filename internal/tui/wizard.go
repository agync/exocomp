package tui

import "fmt"

// The first-run wizard renders as full-screen states. Nothing here writes to
// disk — the steps collect what step 4 will commit, and the copy is explicit
// that quitting before then leaves no trace.

// WizardIdentity is step 1: machine (detected), operator (typed), and the
// generated ntfy topic whose test message is sent at step 4.
type WizardIdentity struct {
	machine  string // "aurora"
	operator string // "Dana Kim" (with a trailing ▍ cursor when focused)
	topic    string // "exo-aurora-8f2"
	cursor   bool   // draw the operator-field cursor
}

func renderWizardIdentity(w WizardIdentity) string {
	op := w.operator
	if w.cursor {
		op += "▍"
	}
	head := []string{lr("exocomp · first run", "step 1 of 4", 80), hrule(80)}
	body := []string{
		"IDENTITY",
		"",
		field(15, "machine", w.machine+" (hostname, detected) ✓"),
		field(15, "operator", op),
		field(15, "notify (ntfy)", "topic "+w.topic+" · generated · test message sent at step 4"),
		"",
		"quitting now leaves no trace — nothing is written until step 4",
	}
	footer := []string{hrule(80), lr("[tab] next field  [enter] continue", "[esc] quit setup", 80)}
	return frame(24, head, body, footer)
}

// field lays a wizard label out in a gutter of the given width with its value.
func field(width int, label, value string) string {
	return fmt.Sprintf("%-*s%s", width, label, value)
}

// WizardKit is step 2: the recovery kit shown once, blocking on the typed
// confirmation phrase. Losing the key means every backup is unreadable, so the
// gate is deliberate friction, not a formality.
type WizardKit struct {
	machine string // "aurora"
	ageKey  string // "AGE-SECRET-KEY-1QL38U…9VK7XW"
	keyID   string // "aurora-2026-08"
	created string // "2026-08-22 14:03 UTC+11"
	typed   string // what the operator has typed toward "I HAVE STORED THIS"
}

// KitConfirmPhrase is the exact phrase the kit step blocks on. It is a
// constant so the wizard and its test agree on the one string that unlocks.
const KitConfirmPhrase = "I HAVE STORED THIS"

func renderWizardKit(w WizardKit) string {
	head := []string{lr("exocomp · first run", "step 2 of 4", 80), hrule(80)}
	card := box(12, 54, []string{
		"exocomp recovery kit · " + w.machine,
		field(10, "age key", w.ageKey),
		field(10, "key id", w.keyID),
		field(10, "created", w.created),
		field(10, "restore", "exocomp restore --key <file>"),
	})
	body := []string{"RECOVERY KIT — shown once. print it or copy it now.", ""}
	body = append(body, card...)
	body = append(body,
		"",
		"without this key, every backup is unreadable — there is no reset",
		"",
		"type exactly to continue:  "+KitConfirmPhrase,
		"> "+w.typed+"▍",
	)
	footer := []string{hrule(80), lr("[esc] back", "enter unlocks when the phrase matches", 80)}
	return frame(24, head, body, footer)
}
