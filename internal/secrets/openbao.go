package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// transitClient speaks the OpenBao (and Vault) Transit HTTP API: POST
// /v1/transit/encrypt/<key> and /v1/transit/decrypt/<key>, authenticated with a
// token header. It wraps and unwraps the DEK without the transit key ever
// leaving OpenBao — the auto-unseal pattern (ADR 0013). No third-party client:
// two endpoints and a bearer header do not earn a dependency.
type transitClient struct {
	addr      string
	token     string
	namespace string
	keyName   string
	http      *http.Client
}

// transitMaxResponse bounds a transit response body. A ciphertext wrapping a
// 32-byte DEK is well under a kilobyte; the cap keeps a misconfigured address
// pointed at something enormous from being read into memory.
const transitMaxResponse = 1 << 16

func (c *transitClient) encrypt(ctx context.Context, plaintext []byte) (string, error) {
	reqBody, err := json.Marshal(map[string]string{
		"plaintext": base64.StdEncoding.EncodeToString(plaintext),
	})
	if err != nil {
		return "", fmt.Errorf("secrets: transit encrypt: marshal: %w", err)
	}
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := c.do(ctx, "encrypt", reqBody, &out); err != nil {
		return "", err
	}
	if out.Data.Ciphertext == "" {
		return "", fmt.Errorf("secrets: transit encrypt: response carried no ciphertext")
	}
	return out.Data.Ciphertext, nil
}

func (c *transitClient) decrypt(ctx context.Context, ciphertext string) ([]byte, error) {
	reqBody, err := json.Marshal(map[string]string{"ciphertext": ciphertext})
	if err != nil {
		return nil, fmt.Errorf("secrets: transit decrypt: marshal: %w", err)
	}
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := c.do(ctx, "decrypt", reqBody, &out); err != nil {
		return nil, err
	}
	dek, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("secrets: transit decrypt: decode plaintext: %w", err)
	}
	return dek, nil
}

// do issues one transit request and decodes the JSON response into out. A
// transport error (OpenBao unreachable) and a non-2xx status both surface as
// explicit errors: Open must never treat an unreachable seal as a reason to
// fall back to a weaker path (ADR 0013).
func (c *transitClient) do(ctx context.Context, op string, body []byte, out any) error {
	url := strings.TrimRight(c.addr, "/") + "/v1/transit/" + op + "/" + c.keyName
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("secrets: transit %s: build request: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", c.token)
	if c.namespace != "" {
		req.Header.Set("X-Vault-Namespace", c.namespace)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("secrets: transit %s: openbao unreachable at %s: %w", op, c.addr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, transitMaxResponse))
	if err != nil {
		return fmt.Errorf("secrets: transit %s: read response: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A transit error body carries {"errors":[...]} — key not found, denied
		// token — never a secret value, so it is safe to surface for diagnosis.
		return fmt.Errorf("secrets: transit %s: openbao returned %s: %s",
			op, resp.Status, strings.TrimSpace(string(respBody)))
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("secrets: transit %s: decode response: %w", op, err)
	}
	return nil
}
