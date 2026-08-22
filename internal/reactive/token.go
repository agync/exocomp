package reactive

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// ApprovalPurpose is the HKDF purpose the decision-link key is derived under.
const ApprovalPurpose = "approval"

// WebhookPurpose is the HKDF purpose a source's hook token and body signature
// key are derived under. The source name is the derivation id, so a token
// opens exactly one source's ingress.
const WebhookPurpose = "webhook-hmac"

// ApprovalPrefix is the path decision links are served under.
const ApprovalPrefix = "/approval/"

// HookPrefix is the path webhook ingress is served under.
const HookPrefix = "/hook/"

// The decisions a link can carry.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

const (
	// tokenMACLen is how much of the HMAC a decision link carries: 24 bytes,
	// 192 bits, 32 base64url characters.
	tokenMACLen = 24
	// hookTokenLen is how much of the derived key the URL path carries. 20
	// hex characters is 80 bits: unguessable, and short enough to fit in an
	// Alertmanager receiver configuration a human has to read back.
	hookTokenLen = 20
)

// ErrBadToken reports a decision link that was not minted here, or was minted
// for another job.
var ErrBadToken = errors.New("reactive: token does not verify")

// Deriver is the part of the secrets envelope this package needs: HKDF
// subkeys. Nothing here holds a secret of its own.
type Deriver interface {
	Derive(purpose, id string) []byte
}

// ApprovalToken returns the decision capability for one job, and the hash the
// approval row stores. The token itself is never persisted: a copy of the
// database approves nothing.
//
// It is an HMAC of the job id under the envelope's approval subkey, so it is
// unforgeable without the envelope and cannot be replayed against another job
// — and it is derivable rather than random, which is what lets the 50%-TTL
// re-publish rebuild the same link without the daemon having kept it in
// memory across a restart. Single use is the approval row's state, not the
// token's secrecy over time: the second tap finds a decided row.
func ApprovalToken(d Deriver, jobID int64) (token, hash string, err error) {
	key := d.Derive(ApprovalPurpose, "")
	if len(key) == 0 {
		return "", "", errors.New("reactive: envelope derived no approval key")
	}
	token = base64.RawURLEncoding.EncodeToString(approvalMAC(key, jobID))
	return token, HashToken(token), nil
}

// VerifyApprovalToken checks that a presented token is this job's, and that it
// matches the hash the row stored. Both comparisons are constant-time: a
// decision link is a bearer capability, and a timing oracle over it is a way
// to forge one.
func VerifyApprovalToken(d Deriver, jobID int64, storedHash, presented string) error {
	key := d.Derive(ApprovalPurpose, "")
	if len(key) == 0 {
		return errors.New("reactive: envelope derived no approval key")
	}
	want := base64.RawURLEncoding.EncodeToString(approvalMAC(key, jobID))
	if subtle.ConstantTimeCompare([]byte(want), []byte(presented)) != 1 {
		return ErrBadToken
	}
	if subtle.ConstantTimeCompare([]byte(HashToken(presented)), []byte(storedHash)) != 1 {
		return ErrBadToken
	}
	return nil
}

// HashToken returns what an approval row stores for a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func approvalMAC(key []byte, jobID int64) []byte {
	mac := hmac.New(sha256.New, key)
	var id [8]byte
	binary.BigEndian.PutUint64(id[:], uint64(jobID))
	mac.Write(id[:])
	return mac.Sum(nil)[:tokenMACLen]
}

// DecisionURL is the link a notification's action button carries. It names no
// host, workspace, or resource: what the job wants to do is behind the tap.
func DecisionURL(baseURL string, approvalID int64, decision, token string) string {
	return fmt.Sprintf("%s%s%d/%s?t=%s", baseURL, ApprovalPrefix, approvalID,
		decision, url.QueryEscape(token))
}

// HookToken is the path segment that identifies a source's ingress. It is
// derived, so the operator recovers it from the envelope rather than from a
// file that has to survive a restore.
func HookToken(d Deriver, source string) string {
	key := d.Derive(WebhookPurpose, source)
	if len(key) == 0 {
		return ""
	}
	return hex.EncodeToString(key)[:hookTokenLen]
}

// HookURL is the address a sender posts to.
func HookURL(baseURL string, d Deriver, source string) string {
	return baseURL + HookPrefix + HookToken(d, source)
}

// VerifyBodySignature checks an X-Exocomp-Signature header against the body,
// under the source's own subkey. Senders that cannot sign (Alertmanager among
// them) present no header and are authenticated by the path token alone; a
// header that is present and wrong is a refusal, never a warning.
func VerifyBodySignature(d Deriver, source, header string, body []byte) error {
	hexMAC, ok := trimSignaturePrefix(header)
	if !ok {
		return fmt.Errorf("reactive: signature header is not sha256=<hex>")
	}
	presented, err := hex.DecodeString(hexMAC)
	if err != nil {
		return fmt.Errorf("reactive: signature is not hex")
	}
	key := d.Derive(WebhookPurpose, source)
	if len(key) == 0 {
		return errors.New("reactive: envelope derived no webhook key")
	}

	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	if subtle.ConstantTimeCompare(mac.Sum(nil), presented) != 1 {
		return fmt.Errorf("reactive: signature does not match the body")
	}
	return nil
}

// SignBody produces the header a sender that can sign should send. It exists
// so the operator's own tooling, and the tests, sign the same way the ingress
// verifies.
func SignBody(d Deriver, source string, body []byte) string {
	mac := hmac.New(sha256.New, d.Derive(WebhookPurpose, source))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func trimSignaturePrefix(header string) (string, bool) {
	const prefix = "sha256="
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return "", false
	}
	return header[len(prefix):], true
}

// parseApprovalID reads the id out of a decision path segment.
func parseApprovalID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("reactive: %q is not an approval id", s)
	}
	return id, nil
}
