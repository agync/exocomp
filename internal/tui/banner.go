package tui

import "fmt"

// BannerState is the top-line status the banner renders. Pause and mute are
// independent: pause holds reactive jobs (events swallowed), mute holds
// notifications while jobs still run. The two booleans give the copy deck's
// three live variants — PAUSED, MUTED, PAUSED + MUTED — and NoColor swaps the
// ▌ marker for the "!!" text form so the state survives with no color.
type BannerState struct {
	Paused         bool
	Swallowed      int
	Muted          bool
	MutedUntil     string // "15:00"
	MutedRemaining string // "43m"
	NoColor        bool
}

// Active reports whether anything is holding the daemon back.
func (s BannerState) Active() bool { return s.Paused || s.Muted }

// bannerLine composes the banner into an 80-column line with the action key
// flush right and a one-column right margin (the fixtures carry that trailing
// space).
func bannerLine(s BannerState) string {
	const w = 80
	marker := "▌"
	if s.NoColor {
		marker = "!!"
	}
	var left, keys string
	switch {
	case s.Paused && s.Muted:
		left = fmt.Sprintf("%s PAUSED + MUTED until %s · %d swallowed", marker, s.MutedUntil, s.Swallowed)
		keys = "[p] resume  [m] unmute"
	case s.Muted:
		left = fmt.Sprintf("%s MUTED until %s (%s) · notifications held · jobs still run",
			marker, s.MutedUntil, s.MutedRemaining)
		keys = "[m] unmute"
	default:
		left = fmt.Sprintf("%s PAUSED — reactive jobs held · %d alerts swallowed", marker, s.Swallowed)
		keys = "[p] resume"
	}
	return lr(left, keys, w-1) + " "
}

// muteModalLines is the mute modal's body: a mandatory-expiry chooser, a logged
// reason, and the promise that everything is loud again by the deadline. It is
// wrapped by box at render time.
type muteModal struct {
	expiry string // "for:  15m   1h   4h  until 09:00"
	reason string // "reason (logged): maintenance window▍"
	until  string // "15:00"
}

func renderMuteModal(m muteModal) string {
	lines := []string{
		"mute notifications",
		"",
		m.expiry,
		m.reason,
		"",
		"everything is loud again by " + m.until + " — no indefinite mute",
		"[enter] mute · [←/→] expiry · [esc] cancel",
	}
	return join(box(1, 56, lines))
}
