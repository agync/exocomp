package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/web"
)

// ntfySeam is the web surface's ntfy leg (web.NtfyDriver): the daemon's
// judgment of the sealed ntfy secrets and a test push through the same
// LoadNtfy transport the daemon builds. The web surface holds no open
// envelope, so both the judgment and the push live behind this seam, the same
// shape as the cordon controller. It re-derives the NewNtfy refusal rule
// (public host + no token, internal/notify/ntfy.go) from the sealed secrets
// this process holds — presence, never values.
type ntfySeam struct {
	store *store.Store
	env   *secrets.Envelope
}

// newNtfySeam wires the seam over the store and envelope both the daemon and
// the standalone web process hold.
func newNtfySeam(st *store.Store, env *secrets.Envelope) *ntfySeam {
	return &ntfySeam{store: st, env: env}
}

// look opens one sealed secret, exactly as the daemon's transport load does.
func (n *ntfySeam) look(ctx context.Context, name string) ([]byte, error) {
	row, err := n.store.GetSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	return n.env.OpenSecret(secrets.SealedSecret{
		Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
	})
}

// secret opens one sealed secret and reports whether it exists at all.
func (n *ntfySeam) secret(ctx context.Context, name string) ([]byte, bool) {
	pt, err := n.look(ctx, name)
	if err != nil {
		return nil, false
	}
	return pt, true
}

// State reports the ntfy leg's judgment: the sealed keys' presence and the
// server class — a fact about the URL, never the URL. Refused re-derives the
// NewNtfy rule: a public server (ntfy.sh or a host under it) without an
// access token.
func (n *ntfySeam) State(ctx context.Context) (web.NtfySeamState, error) {
	var st web.NtfySeamState
	rawURL, urlOK := n.secret(ctx, notify.SecretNtfyURL)
	_, topicOK := n.secret(ctx, notify.SecretNtfyTopic)
	rawToken, tokenOK := n.secret(ctx, notify.SecretNtfyToken)
	st.URLSealed, st.TopicSealed, st.TokenSealed = urlOK, topicOK, tokenOK
	if !urlOK || !topicOK {
		return st, nil
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(string(rawURL)), "/"))
	if err != nil {
		return st, nil
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return st, nil
	}
	if !isPublicNtfyHost(base.Hostname()) {
		st.ServerClass = "self-hosted"
		return st, nil
	}
	st.ServerClass = "public"
	if strings.TrimSpace(string(rawToken)) == "" {
		st.Refused = true
	}
	return st, nil
}

// isPublicNtfyHost re-derives notify's own host classification (ntfy.go:34-37,
// the same rule NewNtfy applies): ntfy.sh or any host under it is the public
// server, normalized first because url.Hostname does not case-fold or strip
// the trailing dot.
func isPublicNtfyHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "ntfy.sh" || strings.HasSuffix(host, ".ntfy.sh")
}

// TestPush sends one test notification through the live transport. A channel
// that cannot load (the NewNtfy refusal) returns the refusal error; a delivery
// failure returns the transport's own. The notification names host and clock
// only — never a secret.
func (n *ntfySeam) TestPush(ctx context.Context, host string) error {
	t, err := notify.LoadNtfy(ctx, n.look)
	if err != nil {
		return fmt.Errorf("notify: test push refused: %w", web.ErrNtfyRefused)
	}
	if t == nil {
		return errors.New("notify: test push: no ntfy channel configured")
	}
	if raw, derr := n.store.GetSetting(ctx, store.SettingNtfyDetail); derr == nil {
		t.SetDetail(notify.Detail(raw))
	}
	now := time.Now().UTC()
	return t.Publish(ctx, notify.Notification{
		Title:    "exocomp: test push",
		Body:     "exocomp: test push · " + host + " · " + now.Format("15:04:05"),
		Priority: notify.PriorityDefault,
		Source:   "system",
		Host:     host,
	})
}
