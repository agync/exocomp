package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

// TransitSealName is the file holding the DEK wrapped by an OpenBao Transit key.
// It rides beside keyring.age and is purely additive: keyring.age still wraps
// the DEK to the age recipients (host + recovery), so the printed recovery card
// opens the store with no OpenBao in reach (ADR 0013). The blob is transit
// ciphertext ("vault:v1:…"), safe to keep at rest — the transit key never lands
// on host disk.
const TransitSealName = "keyring.transit"

const transitSealFile = TransitSealName

// Seal env vars. The token is read from the environment or a file OUTSIDE the
// data dir and is never written into it — only the transit ciphertext is.
const (
	// EnvSealAddress is the OpenBao API address, e.g. https://bao.lan:8200.
	EnvSealAddress = "EXOCOMP_SEAL_TRANSIT_ADDRESS"
	// EnvSealToken carries the OpenBao token directly.
	EnvSealToken = "EXOCOMP_SEAL_TRANSIT_TOKEN"
	// EnvSealTokenFile points at a file holding the token, for an operator who
	// would rather not put it in the environment. It must sit outside the data
	// volume.
	EnvSealTokenFile = "EXOCOMP_SEAL_TRANSIT_TOKEN_FILE"
	// EnvSealKey is the transit key name that wraps the DEK.
	EnvSealKey = "EXOCOMP_SEAL_TRANSIT_KEY"
	// EnvSealNamespace is the optional OpenBao/Vault namespace (enterprise).
	EnvSealNamespace = "EXOCOMP_SEAL_TRANSIT_NAMESPACE"
)

// transitTimeout bounds one wrap/unwrap round trip so a black-holed address
// fails Open with a clear error instead of hanging the daemon at boot.
const transitTimeout = 15 * time.Second

// SealConfig points the envelope at an OpenBao Transit key that wraps the DEK.
// It is optional: a nil or unconfigured SealConfig leaves Open on the age
// host-key path, byte-for-byte as before (ADR 0013).
type SealConfig struct {
	Address   string
	Token     string
	KeyName   string
	Namespace string
	// HTTPClient is for tests to inject an httptest server. Production leaves it
	// nil and a timeout-bounded default is used.
	HTTPClient *http.Client
}

// Configured reports whether enough is set to reach a transit key. A partially
// filled config is a configuration error surfaced by SealConfigFromEnv, not a
// silent unconfigured state.
func (c *SealConfig) Configured() bool {
	return c != nil && c.Address != "" && c.Token != "" && c.KeyName != ""
}

// SealConfigFromEnv reads the seal configuration from the environment. It
// returns (nil, nil) when no seal env var is set — the unconfigured default. It
// returns an explicit error when the seal is partially configured, so a
// half-set seal fails loudly rather than degrading to the on-disk key.
func SealConfigFromEnv() (*SealConfig, error) {
	addr := strings.TrimSpace(os.Getenv(EnvSealAddress))
	key := strings.TrimSpace(os.Getenv(EnvSealKey))
	namespace := strings.TrimSpace(os.Getenv(EnvSealNamespace))
	tokenEnv := os.Getenv(EnvSealToken)
	tokenFile := strings.TrimSpace(os.Getenv(EnvSealTokenFile))

	if addr == "" && key == "" && namespace == "" && tokenEnv == "" && tokenFile == "" {
		return nil, nil
	}

	token, err := resolveSealToken(tokenEnv, tokenFile)
	if err != nil {
		return nil, err
	}
	var missing []string
	if addr == "" {
		missing = append(missing, EnvSealAddress)
	}
	if key == "" {
		missing = append(missing, EnvSealKey)
	}
	if token == "" {
		missing = append(missing, EnvSealToken+" or "+EnvSealTokenFile)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("secrets: openbao seal is partially configured; also set %s",
			strings.Join(missing, ", "))
	}
	return &SealConfig{Address: addr, Token: token, KeyName: key, Namespace: namespace}, nil
}

func resolveSealToken(tokenEnv, tokenFile string) (string, error) {
	if tokenEnv != "" {
		return tokenEnv, nil
	}
	if tokenFile == "" {
		return "", nil
	}
	body, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("secrets: read %s=%s: %w", EnvSealTokenFile, tokenFile, err)
	}
	token := strings.TrimRight(string(body), "\r\n")
	if token == "" {
		return "", fmt.Errorf("secrets: %s=%s is empty", EnvSealTokenFile, tokenFile)
	}
	return token, nil
}

func (c *SealConfig) client() *transitClient {
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: transitTimeout}
	}
	return &transitClient{
		addr:      c.Address,
		token:     c.Token,
		namespace: c.Namespace,
		keyName:   c.KeyName,
		http:      hc,
	}
}

// OpenWithSeal opens the envelope, using an OpenBao Transit seal when one is
// configured and the age host-key path otherwise. With a nil or unconfigured
// seal it is exactly Open: same reads, same result, so the unconfigured path is
// unchanged (ADR 0013).
//
// When the seal is configured, the DEK is unwrapped from keyring.transit via
// transit/decrypt — no host key needed, the auto-unseal value. An unreachable
// OpenBao is an explicit error, never a silent fall back to the host-key path
// that would weaken the seal.
func OpenWithSeal(dataDir string, seal *SealConfig) (*Envelope, error) {
	if !seal.Configured() {
		return Open(dataDir)
	}
	return openTransit(dataDir, seal)
}

func openTransit(dataDir string, seal *SealConfig) (*Envelope, error) {
	blob, err := os.ReadFile(filepath.Join(dataDir, transitSealFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf(
			"secrets: openbao seal is configured but %s is absent; run `exocomp seal enable` "+
				"or unset %s to open with the host key", transitSealFile, EnvSealAddress)
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: read transit seal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), transitTimeout)
	defer cancel()
	dek, err := seal.client().decrypt(ctx, strings.TrimSpace(string(blob)))
	if err != nil {
		return nil, err
	}
	if len(dek) != dekLen {
		clear(dek)
		return nil, fmt.Errorf("secrets: transit seal holds a %d-byte key, want %d", len(dek), dekLen)
	}

	// keyring.age is still present and still wraps this same DEK to the age
	// recipients: it is read for its ciphertext (KeyringAge, backup targets),
	// not to unwrap the DEK. The host key is optional here — auto-unseal exists
	// precisely so it need not be on disk.
	keyring, err := os.ReadFile(filepath.Join(dataDir, keyringFile))
	if err != nil {
		clear(dek)
		return nil, fmt.Errorf("secrets: read keyring: %w", err)
	}
	recipients, err := readRecipients(filepath.Join(dataDir, recipientsFile))
	if err != nil {
		clear(dek)
		return nil, err
	}
	version, err := ReadKeyringVersion(dataDir)
	if err != nil {
		clear(dek)
		return nil, err
	}
	host, protection := loadHostIdentityOptional(dataDir)

	env := &Envelope{
		dataDir:     dataDir,
		dek:         dek,
		keyring:     keyring,
		host:        host,
		recipients:  recipients,
		version:     version,
		hostKeyProt: protection,
	}
	env.lockDEK()
	return env, nil
}

// loadHostIdentityOptional resolves the host identity if it is present, and
// reports absence rather than failing: the transit path does not require it. A
// nil *age.X25519Identity must not be returned as a non-nil age.Identity, so the
// interface value is set only on success.
func loadHostIdentityOptional(dataDir string) (age.Identity, HostKeyProtection) {
	id, protection, err := loadHostIdentity(dataDir)
	if err != nil {
		return nil, HostKeyAbsent
	}
	return id, protection
}

// EnableTransitSeal wraps the live DEK with the configured transit key and
// writes keyring.transit beside keyring.age. It is additive: keyring.age is left
// untouched, so the recovery card still opens the store without OpenBao. Calling
// it again re-wraps (e.g. after a transit key rotation on OpenBao's side).
func EnableTransitSeal(ctx context.Context, env *Envelope, seal *SealConfig) error {
	if !seal.Configured() {
		return errors.New("secrets: seal enable: openbao seal is not configured")
	}
	if len(env.dek) != dekLen {
		return ErrClosed
	}
	ciphertext, err := seal.client().encrypt(ctx, env.dek)
	if err != nil {
		return err
	}
	path := filepath.Join(env.dataDir, transitSealFile)
	if err := writeFileAtomic(path, []byte(ciphertext+"\n"), 0o600); err != nil {
		return fmt.Errorf("secrets: seal enable: write %s: %w", transitSealFile, err)
	}
	return nil
}

// DisableTransitSeal removes keyring.transit. The DEK stays wrapped in
// keyring.age, so the daemon reverts to opening with the host key. Removing a
// file that is already absent is not an error.
func DisableTransitSeal(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, transitSealFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("secrets: seal disable: remove %s: %w", transitSealFile, err)
	}
	return nil
}

// TransitSealPresent reports whether a transit seal blob exists in dataDir, for
// doctor and the seal status command to state the posture without unwrapping.
func TransitSealPresent(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, transitSealFile))
	return err == nil
}
