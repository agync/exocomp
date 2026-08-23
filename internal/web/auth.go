package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// sessionCookie is the name of the browser session cookie. It is HttpOnly and
// SameSite=Strict: the phone approval surface never wants a cookie riding a
// cross-site request, and script never needs to read it.
const sessionCookie = "exocomp_session"

// sessionTTL bounds a browser session. It is short because re-bootstrapping
// from a token is one paste; a long-lived cookie is a credential left lying
// around. A session never outlives its token's own expiry either.
const sessionTTL = 12 * time.Hour

// sessionPurpose is the HKDF purpose the cookie-signing key is derived under.
// It is stable across restarts and survives a restore, so a session minted
// before a reboot still verifies after it — until its token is revoked.
const sessionPurpose = "web-session"

// ErrUnauthenticated reports a request that carried no usable credential.
var ErrUnauthenticated = errors.New("web: no valid session or token")

// ErrAuthThrottled reports that this source (or this token row) has failed
// authentication too many times in a row and is serving a backoff, so the
// attempt was refused without running the KDF (finding AI-11). It is a
// transient 429, not a permanent lockout.
var ErrAuthThrottled = errors.New("web: too many failed authentications, retry later")

// ErrKDFBusy reports that the pre-auth KDF gate is saturated: too many argon2id
// verifications are already in flight, so this one was refused rather than run
// (finding D1). It is a transient 503, not an authentication failure.
var ErrKDFBusy = errors.New("web: authentication is busy, retry shortly")

// identity is the resolved caller behind a request: the token row that
// authenticated it. Every audited action names it (ADR 0009 §5).
type identity struct {
	token store.APIToken
	via   string // "session" or "bearer"
}

type ctxKey int

const identityKey ctxKey = 0

func withIdentity(ctx context.Context, id identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// identityOf returns the caller resolved for a request, and whether one was.
func identityOf(ctx context.Context) (identity, bool) {
	id, ok := ctx.Value(identityKey).(identity)
	return id, ok
}

// sessionKey is the HMAC key the session cookie is signed under.
func (s *Server) sessionKey() []byte {
	return s.deriver.Derive(sessionPurpose, "")
}

// mintSession returns the signed cookie value for a token row, good until the
// sooner of the session TTL and the token's own expiry.
func (s *Server) mintSession(t store.APIToken, now time.Time) (value string, expiry time.Time) {
	expiry = now.Add(sessionTTL)
	if t.ExpiresAt != nil && t.ExpiresAt.Before(expiry) {
		expiry = *t.ExpiresAt
	}
	payload := fmt.Sprintf("%d.%d", t.ID, expiry.UnixMilli())
	mac := s.signSession(payload)
	return payload + "." + mac, expiry
}

func (s *Server) signSession(payload string) string {
	mac := hmac.New(sha256.New, s.sessionKey())
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// resolveSession validates a cookie value and returns the token id it names.
// The signature is checked before anything is parsed as a number, so a forged
// cookie never reaches the database.
func (s *Server) resolveSession(value string, now time.Time) (int64, error) {
	i := strings.LastIndex(value, ".")
	if i < 0 {
		return 0, ErrUnauthenticated
	}
	payload, mac := value[:i], value[i+1:]
	want := s.signSession(payload)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(want)) != 1 {
		return 0, ErrUnauthenticated
	}
	fields := strings.SplitN(payload, ".", 2)
	if len(fields) != 2 {
		return 0, ErrUnauthenticated
	}
	id, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, ErrUnauthenticated
	}
	expMillis, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, ErrUnauthenticated
	}
	if !now.Before(time.UnixMilli(expMillis)) {
		return 0, ErrUnauthenticated
	}
	return id, nil
}

// authenticateToken resolves a presented bearer token to its row, verifying the
// argon2id hash and that the row still authenticates. A revoked or expired
// token fails here, which is what makes "session dies with its token" true: the
// session cookie only names a row, and this is the check that reads it.
func (s *Server) authenticateToken(ctx context.Context, presented string, now time.Time) (store.APIToken, error) {
	id, secret, err := parseToken(presented)
	if err != nil {
		return store.APIToken{}, err
	}
	return s.authenticateRow(ctx, id, secret, now)
}

func (s *Server) authenticateRow(ctx context.Context, id int64, secret string, now time.Time) (store.APIToken, error) {
	row, err := s.store.GetToken(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.APIToken{}, s.equalize(secret)
	}
	if err != nil {
		return store.APIToken{}, err
	}
	if !row.Active(now) {
		return store.APIToken{}, s.equalize(secret)
	}
	if secret != "" {
		ok, err := s.verify(secret, row.Hash)
		if err != nil {
			return store.APIToken{}, err
		}
		if !ok {
			return store.APIToken{}, ErrUnauthenticated
		}
	}
	return row, nil
}

// equalize pays the same argon2id a live row's verification would, so an
// unknown or inactive token id is not distinguishable from a live one by
// response time (finding AI-11: 18ms for a live row vs 513µs for a missing one
// enumerated which rows existed). The session path presents no secret and runs
// no KDF either way, so it is left alone.
func (s *Server) equalize(secret string) error {
	if secret != "" {
		_, _ = s.verify(secret, decoyHash())
	}
	return ErrUnauthenticated
}

// decoyHash is a real argon2id PHC string with the production parameters, hashed
// once from a random secret no caller can present.
func decoyHash() string {
	decoyOnce.Do(func() {
		buf := make([]byte, 24)
		if _, err := rand.Read(buf); err != nil {
			// A decoy that cannot be minted still has to have the right shape;
			// a fixed string keeps the failure path constant-cost.
			decoy = fixedDecoyHash
			return
		}
		h, err := hashSecret(base64.RawURLEncoding.EncodeToString(buf))
		if err != nil {
			decoy = fixedDecoyHash
			return
		}
		decoy = h
	})
	return decoy
}

var (
	decoyOnce sync.Once
	decoy     string
)

// fixedDecoyHash is the fallback decoy: a valid PHC string over an unknowable
// secret. verifySecret runs the same KDF against it as against a real row.
const fixedDecoyHash = "$argon2id$v=19$m=65536,t=3,p=4$" +
	"AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// authenticateTokenGated is authenticateToken behind the KDF concurrency gate.
// A malformed token is rejected before a slot is taken — it never reaches the
// KDF — and a saturated gate returns ErrKDFBusy instead of running argon2id, so
// an unauthenticated flood cannot amplify the KDF's memory cost (finding D1).
func (s *Server) authenticateTokenGated(r *http.Request, presented string, now time.Time) (store.APIToken, error) {
	id, secret, err := parseToken(presented)
	if err != nil {
		return store.APIToken{}, err
	}
	// A source (or a token row) that has failed repeatedly is serving a backoff:
	// refuse before the KDF, so a guessing run costs the daemon nothing and
	// makes no progress (finding AI-11).
	src, tok := failKeys(r, id)
	if _, blocked := s.fails.blocked(now, src, tok); blocked {
		return store.APIToken{}, ErrAuthThrottled
	}
	release, ok := s.kdf.acquire(sourceKey(r))
	if !ok {
		return store.APIToken{}, ErrKDFBusy
	}
	defer release()

	row, err := s.authenticateRow(r.Context(), id, secret, now)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		if n := s.fails.fail(now, src, tok); n == authFailThreshold+1 {
			// Once per run, not once per attempt: the operator should see a
			// guessing run in the log without it becoming the log.
			s.log.Warn("web: repeated authentication failures; backing off",
				"source", sourceKey(r), "token_id", id, "failures", n)
		}
	case err == nil:
		s.fails.succeed(src, tok)
	}
	return row, err
}

// failKeys are the two namespaces a failure counts against: the peer address and
// the token row it was presented for.
func failKeys(r *http.Request, id int64) (source, token string) {
	return "src:" + sourceKey(r), "tok:" + strconv.FormatInt(id, 10)
}

// resolve authenticates a request by session cookie first, then by a bearer
// header. It returns the identity without mutating anything; the caller decides
// whether the resolved scope may do what the request asks.
func (s *Server) resolve(r *http.Request) (identity, error) {
	now := s.now()
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		id, err := s.resolveSession(c.Value, now)
		if err == nil {
			// The session names a row; the row is the authority on whether it
			// still authenticates, so a revoke reaches the cookie here.
			row, err := s.authenticateRow(r.Context(), id, "", now)
			if err == nil {
				return identity{token: row, via: "session"}, nil
			}
			if !errors.Is(err, ErrUnauthenticated) {
				return identity{}, err
			}
		}
	}

	if h := r.Header.Get("Authorization"); h != "" {
		presented, ok := strings.CutPrefix(h, "Bearer ")
		if ok {
			row, err := s.authenticateTokenGated(r, strings.TrimSpace(presented), now)
			if err == nil {
				return identity{token: row, via: "bearer"}, nil
			}
			if !errors.Is(err, ErrUnauthenticated) && !errors.Is(err, ErrMalformedToken) {
				return identity{}, err
			}
		}
	}

	return identity{}, ErrUnauthenticated
}

// requireAuth wraps a handler so every request behind it carries a resolved
// identity. An unauthenticated browser request is redirected to the login
// form; an unauthenticated API request gets 401. The token is touched once per
// authenticated request so `token list` shows a real last-used.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := s.resolve(r)
		if err != nil {
			if errors.Is(err, ErrKDFBusy) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "authentication busy, retry shortly", http.StatusServiceUnavailable)
				return
			}
			if errors.Is(err, ErrAuthThrottled) {
				w.Header().Set("Retry-After", strconv.Itoa(int(authFailBase.Seconds())))
				http.Error(w, "too many failed authentications, retry later", http.StatusTooManyRequests)
				return
			}
			if !errors.Is(err, ErrUnauthenticated) {
				s.log.Error("web: authenticate", "err", err)
				http.Error(w, "authentication error", http.StatusInternalServerError)
				return
			}
			if wantsHTML(r) {
				http.Redirect(w, r, "/login?next="+urlEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="exocomp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := s.store.TouchToken(r.Context(), id.token.ID); err != nil {
			s.log.Warn("web: touch token", "token", id.token.Name, "err", err)
		}
		next(w, r.WithContext(withIdentity(r.Context(), id)))
	}
}

// mayApprove reports whether a resolved scope is allowed to decide approvals on
// the BROWSER surface. readonly may not; operator and approve may — that surface
// is the operator at the console. The machine API is stricter (canApprove: the
// approve scope alone), because a bearer token is the thing handed to an agent.
func mayApprove(id identity) bool {
	return id.token.Scope == store.ScopeOperator || id.token.Scope == store.ScopeApprove
}

// decidedBySelf reports whether the caller is the principal a gated job's ledger
// records as having asked for it. Separation of duty is the whole point of the
// gate: whoever raised it may not release it (finding AI-2). A CLI-enqueued job
// records no actor, so the local operator's own workflow is unchanged; only a
// token-attributed write is bound by this.
func (s *Server) decidedBySelf(ctx context.Context, jobID int64, id identity) (bool, error) {
	actor, err := s.service().JobActor(ctx, jobID)
	if err != nil {
		return false, err
	}
	return actor.Kind == service.ActorKindToken && actor.Name == id.token.Name, nil
}

// RequireTokenForBind enforces ADR 0009 point 1: a non-loopback listener needs
// at least one active token, unless the operator opted into cleartext with
// --insecure-http (which is loud, never silent). A loopback bind is always
// allowed. It returns the error a refused bind reports and a warning to print
// for an insecure bind.
func RequireTokenForBind(ctx context.Context, st *store.Store, addr string, insecure bool, now time.Time) (warn string, err error) {
	if isLoopbackBind(addr) {
		return "", nil
	}
	active, err := st.CountActiveTokens(ctx, now)
	if err != nil {
		return "", err
	}
	if active == 0 {
		return "", fmt.Errorf("web: refusing to bind non-loopback address %q with no active token: "+
			"mint one with `exocomp token new <name>` (ADR 0009)", addr)
	}
	if insecure {
		return "WARNING: --insecure-http serves the LAN surface over cleartext; " +
			"a token on the wire is readable by anyone on the network. Use TLS " +
			"(the default) or a Tailscale/Caddy front instead.", nil
	}
	return "", nil
}

// isLoopbackBind reports whether a bind address is loopback-only. An empty host
// or a wildcard is not loopback: it reaches the LAN.
func isLoopbackBind(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname like "localhost" resolves to loopback; anything else we
		// cannot prove is loopback, so we treat it as LAN and require a token.
		return strings.EqualFold(host, "localhost")
	}
	return ip.IsLoopback()
}

func wantsHTML(r *http.Request) bool {
	if r.Header.Get("HX-Request") == "true" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html") ||
		r.Header.Get("Accept") == ""
}
