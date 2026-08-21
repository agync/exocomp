package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// The S3 leg needs exactly two object operations — put and get keyring.age and
// RECOVERY.md at the bucket root — so it signs them itself instead of pulling
// in an SDK. restic does all the repository work.

const (
	s3Service   = "s3"
	s3Algorithm = "AWS4-HMAC-SHA256"
	s3Timeout   = 2 * time.Minute
)

type s3Client struct {
	endpoint *url.URL
	bucket   string
	prefix   string
	region   string
	key      string
	secret   string
	token    string
	now      func() time.Time
	http     *http.Client
}

// newS3Client parses restic's s3 URI form — s3:<endpoint>/<bucket>[/<prefix>],
// where an endpoint without a scheme means https — and takes its credentials
// from the target's own environment map.
func newS3Client(uri string, env map[string]string) (*s3Client, error) {
	rest, ok := strings.CutPrefix(uri, "s3:")
	if !ok {
		return nil, fmt.Errorf("backup: s3 uri %q is missing the s3 scheme prefix", uri)
	}
	if !strings.Contains(rest, "://") {
		rest = "https://" + rest
	}
	parsed, err := url.Parse(rest)
	if err != nil {
		return nil, fmt.Errorf("backup: parse s3 uri %q: %w", uri, err)
	}

	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return nil, fmt.Errorf("backup: s3 uri %q names no bucket", uri)
	}

	c := &s3Client{
		endpoint: &url.URL{Scheme: parsed.Scheme, Host: parsed.Host},
		bucket:   segments[0],
		prefix:   strings.Join(segments[1:], "/"),
		region:   firstNonEmpty(env["AWS_DEFAULT_REGION"], env["AWS_REGION"], "us-east-1"),
		key:      env["AWS_ACCESS_KEY_ID"],
		secret:   env["AWS_SECRET_ACCESS_KEY"],
		token:    env["AWS_SESSION_TOKEN"],
		now:      func() time.Time { return time.Now().UTC() },
		http:     &http.Client{Timeout: s3Timeout},
	}
	if c.key == "" || c.secret == "" {
		return nil, fmt.Errorf("backup: target %q has no AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY", uri)
	}
	return c, nil
}

func (c *s3Client) objectURL(key string) string {
	full := path.Join(c.bucket, c.prefix, key)
	return c.endpoint.String() + "/" + uriEncode(full, false)
}

func (c *s3Client) put(ctx context.Context, key string, body []byte, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.objectURL(key), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("backup: s3 put %s: %w", key, err)
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	if err := c.sign(req, body); err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("backup: s3 put %s: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("backup: s3 put %s: %s: %s", key, resp.Status, snippet(resp.Body))
	}
	return nil
}

func (c *s3Client) get(ctx context.Context, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.objectURL(key), nil)
	if err != nil {
		return nil, fmt.Errorf("backup: s3 get %s: %w", key, err)
	}
	if err := c.sign(req, nil); err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backup: s3 get %s: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("backup: s3 get %s: %s: %s", key, resp.Status, snippet(resp.Body))
	}
	return io.ReadAll(resp.Body)
}

// sign applies AWS Signature Version 4 to req over the exact payload bytes.
func (c *s3Client) sign(req *http.Request, payload []byte) error {
	now := c.now()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := hex.EncodeToString(sha256sum(payload))

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if c.token != "" {
		req.Header.Set("X-Amz-Security-Token", c.token)
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	values := map[string]string{"host": req.Host}
	for _, h := range []string{"Content-Type", "X-Amz-Content-Sha256", "X-Amz-Date", "X-Amz-Security-Token"} {
		v := req.Header.Get(h)
		if v == "" {
			continue
		}
		name := strings.ToLower(h)
		values[name] = strings.TrimSpace(v)
		if name != "x-amz-content-sha256" && name != "x-amz-date" {
			signed = append(signed, name)
		}
	}
	sort.Strings(signed)

	var canonicalHeaders strings.Builder
	for _, h := range signed {
		canonicalHeaders.WriteString(h + ":" + values[h] + "\n")
	}
	signedHeaders := strings.Join(signed, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, c.region, s3Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		s3Algorithm,
		amzDate,
		scope,
		hex.EncodeToString(sha256sum([]byte(canonicalRequest))),
	}, "\n")

	k := hmacSHA256([]byte("AWS4"+c.secret), dateStamp)
	k = hmacSHA256(k, c.region)
	k = hmacSHA256(k, s3Service)
	k = hmacSHA256(k, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(k, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s3Algorithm, c.key, scope, signedHeaders, signature))
	return nil
}

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// uriEncode implements the AWS canonical URI encoding: unreserved characters
// pass through, everything else becomes uppercase percent escapes.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~':
			b.WriteByte(r)
		case r == '/':
			if encodeSlash {
				b.WriteString("%2F")
			} else {
				b.WriteByte(r)
			}
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func snippet(r io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(body))
}

// s3CredField is one field of a restic_s3 target's sealed credential,
// mapping the sealed secret row's name suffix to the environment variable it
// fills.
type s3CredField struct {
	suffix string
	envKey string
}

var s3CredFields = []s3CredField{
	{"access-key-id", "AWS_ACCESS_KEY_ID"},
	{"secret-access-key", "AWS_SECRET_ACCESS_KEY"},
	{"region", "AWS_DEFAULT_REGION"},
	{"session-token", "AWS_SESSION_TOKEN"},
}

// s3SecretName is the sealed secret row name for one credential field of a
// restic_s3 target: "s3/<target>/access-key-id" and so on.
func s3SecretName(target, suffix string) string {
	return "s3/" + target + "/" + suffix
}

// resolveS3Env builds the process environment for a restic_s3 leg. Sealed
// secret rows named s3/<target>/<field> take precedence, field by field,
// over fallback (ordinarily the process environment the caller gathered
// before the store or the envelope were involved); a target with no sealed
// access-key-id/secret-access-key pair falls back to fallback entirely, and
// warned reports that so the caller can log it without ever holding a
// credential value past this call. Fallback keys resolveS3Env does not
// itself resolve (RESTIC_CACHE_DIR in tests, for instance) ride along either
// way.
func resolveS3Env(
	ctx context.Context, st *store.Store, env *secrets.Envelope, target string, fallback map[string]string,
) (out map[string]string, warned bool, err error) {
	resolved := map[string]string{}
	for _, f := range s3CredFields {
		value, found, err := openS3Secret(ctx, st, env, target, f.suffix)
		if err != nil {
			return nil, false, err
		}
		if found {
			resolved[f.envKey] = value
		}
	}

	if resolved["AWS_ACCESS_KEY_ID"] == "" || resolved["AWS_SECRET_ACCESS_KEY"] == "" {
		return fallback, true, nil
	}
	for k, v := range fallback {
		if _, exists := resolved[k]; !exists {
			resolved[k] = v
		}
	}
	return resolved, false, nil
}

// openS3Secret reads and unseals one sealed credential field for target. It
// reports found=false, not an error, for a field that was never sealed —
// only region and session-token are optional, but the caller decides which
// fields are required.
func openS3Secret(
	ctx context.Context, st *store.Store, env *secrets.Envelope, target, field string,
) (value string, found bool, err error) {
	name := s3SecretName(target, field)
	row, err := st.GetSecret(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("backup: load %s: %w", name, err)
	}
	pt, err := env.OpenSecret(secrets.SealedSecret{
		Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
	})
	if err != nil {
		return "", false, fmt.Errorf("backup: open %s: %w", name, err)
	}
	return string(pt), true, nil
}
