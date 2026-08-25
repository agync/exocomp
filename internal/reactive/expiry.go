package reactive

import (
	"context"
	"fmt"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// SourceSecretExpiry attributes the checkpoint event a secret-expiry page leaves
// behind, and is the Source every expiry notification carries so a scoped mute
// keyed on the secret name can hold it. Like SourceDrift and SourceJobOutcome it
// names an internally raised condition, not a webhook.
const SourceSecretExpiry = "secret-expiry"

// expiryLeadWindow is how far ahead of the deadline a secret is paged on as a
// lead warning, so a rotation can be scheduled before the value stops working
// rather than after. Seven days is one on-call handoff plus a weekend.
const expiryLeadWindow = 7 * 24 * time.Hour

// expiry phases. Each is a distinct condition paged once: the lead warning when
// the secret enters the window, and the expiry itself when the deadline passes.
// They are separate so the lead warning never suppresses the higher-priority
// expired page for the same secret.
const (
	expiryPhaseExpiring = "expiring"
	expiryPhaseExpired  = "expired"
)

// SweepSecretExpiry pages the operator once for every secret whose expiry has
// passed (high priority) and once as a lead warning for every secret expiring
// within expiryLeadWindow (lower priority). A secret that never expires or whose
// deadline is further out than the window pages nothing.
//
// Its idempotency is SweepJobOutcomes': the event table is the checkpoint. A
// page's dedup key names the secret, the phase, and the deadline it paged on, so
// the same condition never pages twice across repeated sweeps or a daemon
// restart, while a secret rotated to a fresh deadline is a new condition that can
// page again.
func (e *Engine) SweepSecretExpiry(ctx context.Context) (int, error) {
	secrets, err := e.store.ListSecrets(ctx)
	if err != nil {
		return 0, err
	}

	now := e.now()
	raised := 0
	for _, sec := range secrets {
		if sec.ExpiresAt == nil {
			continue
		}
		phase, ok := expiryPhase(*sec.ExpiresAt, now)
		if !ok {
			continue
		}

		key := secretExpiryDedupKey(phase, sec.Name, *sec.ExpiresAt)
		seen, err := e.store.CountEventsByDedupKey(ctx, key)
		if err != nil {
			return raised, err
		}
		if seen > 0 {
			continue
		}

		e.notify(ctx, secretExpiryNotification(sec, phase, now))

		// The checkpoint row is written after the page, so a crash between the
		// two re-pages once on the next sweep — louder than a lost alert, the
		// trade SweepJobOutcomes and SweepDrift both make.
		if _, err := e.store.CreateEvent(ctx, store.Event{
			Source:     SourceSecretExpiry,
			ReceivedAt: now,
			DedupKey:   key,
			Outcome:    store.OutcomeMatched,
			Reason:     fmt.Sprintf("secret %q %s; operator paged", sec.Name, phase),
		}); err != nil {
			return raised, err
		}
		raised++
	}
	return raised, nil
}

// expiryPhase reports which condition a deadline is in relative to now: expired
// when it has passed, expiring when it is within the lead window, and not ok when
// it is further out than the window and nothing should page.
func expiryPhase(expiresAt, now time.Time) (string, bool) {
	if !now.Before(expiresAt) {
		return expiryPhaseExpired, true
	}
	if now.Add(expiryLeadWindow).After(expiresAt) {
		return expiryPhaseExpiring, true
	}
	return "", false
}

// secretExpiryDedupKey names the secret, the phase, and the deadline a page
// reported on. The deadline is part of the key so a secret rotated to a new
// expiry is a fresh condition rather than one silenced by the prior page.
func secretExpiryDedupKey(phase, name string, expiresAt time.Time) string {
	return fmt.Sprintf("%s:%s:%s:%d", SourceSecretExpiry, phase, name, expiresAt.UnixMilli())
}

// secretExpiryNotification is the page a deadline raises: high priority once the
// secret has expired, lower for the lead warning, attributed by secret name so a
// scoped mute can hold it. It names the secret and its deadline, never its value.
// now is the sweep's own clock, so the ttl tag reflects the engine's fake clock
// under test rather than the wall clock.
func secretExpiryNotification(sec store.SecretRow, phase string, now time.Time) notify.Notification {
	when := sec.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")
	click := fmt.Sprintf("exocomp://secrets/%s", sec.Name)
	idTag := fmt.Sprintf("id:%s", sec.Name)
	if phase == expiryPhaseExpired {
		return notify.Notification{
			Title:    "secret expired: " + sec.Name,
			Body:     fmt.Sprintf("Secret %q (%s) expired at %s. Rotate or remove it.", sec.Name, sec.Kind, when),
			Priority: notify.PriorityHigh,
			Tags:     []string{"rotating_light", idTag, "ttl:0", "kind:expiry"},
			Click:    click,
			Source:   SourceSecretExpiry,
			Host:     sec.Name,
		}
	}
	return notify.Notification{
		Title:    "secret expiring: " + sec.Name,
		Body:     fmt.Sprintf("Secret %q (%s) expires at %s. Rotate it before then.", sec.Name, sec.Kind, when),
		Priority: notify.PriorityLow,
		Tags: []string{"hourglass", idTag,
			fmt.Sprintf("ttl:%d", ttlSeconds(*sec.ExpiresAt, now)), "kind:expiry"},
		Click:  click,
		Source: SourceSecretExpiry,
		Host:   sec.Name,
	}
}

// ttlSeconds renders how long is left until deadline, clamped at zero: a
// notification's ttl tag is never negative even if the sweep is running a
// beat behind the clock.
func ttlSeconds(deadline, now time.Time) int64 {
	left := int64(deadline.Sub(now).Seconds())
	if left < 0 {
		return 0
	}
	return left
}
