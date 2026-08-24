package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// Environment and file names for the host-key-at-rest tiers. The default
// (non-TPM) story has to be genuinely secure: a stolen data volume must not
// contain the key that unwraps the DEK.
const (
	// EnvHostKeyPassphrase carries the operator passphrase that wraps host.key
	// on the compose / non-systemd tier. It lives outside the data volume.
	EnvHostKeyPassphrase = "EXOCOMP_HOST_KEY_PASSPHRASE"
	// EnvHostKeyPassphraseFile points at a file holding that passphrase, for an
	// operator who would rather not put it in the environment. It too must sit
	// outside the data volume.
	EnvHostKeyPassphraseFile = "EXOCOMP_HOST_KEY_PASSPHRASE_FILE"
	// EnvCredentialsDirectory is systemd's $CREDENTIALS_DIRECTORY. When it holds
	// host.key, systemd has already decrypted the credential into it — the
	// unwrap secret (/var/lib/systemd/credential.secret) never entered the data
	// volume, so this is the secure default on the systemd tier.
	EnvCredentialsDirectory = "CREDENTIALS_DIRECTORY"

	// hostKeyAgeFile is the scrypt-wrapped host key on the passphrase tier.
	hostKeyAgeFile = "host.key.age"
	// tpmResourceMgr is the in-kernel TPM 2.0 resource-manager device. Its
	// presence is how TPM sealing is detected rather than assumed.
	tpmResourceMgr = "/dev/tpmrm0"
)

// HostKeyProtection is the at-rest protection of the host identity, reported
// honestly by init and doctor rather than assumed.
type HostKeyProtection int

const (
	// HostKeyAbsent means no host key was found in any tier.
	HostKeyAbsent HostKeyProtection = iota
	// HostKeyPlaintext is a bare host.key in the data dir: a data-volume read
	// alone yields it. This is the insecure state the fix warns loudly about.
	HostKeyPlaintext
	// HostKeyPassphrase is a scrypt-wrapped host.key.age, unlocked by an
	// operator passphrase held outside the data volume.
	HostKeyPassphrase
	// HostKeySystemdCredential is a host key delivered through systemd's
	// encrypted credential store, unwrapped with a secret outside the volume.
	HostKeySystemdCredential
)

func (p HostKeyProtection) String() string {
	switch p {
	case HostKeyPlaintext:
		return "plaintext-in-datadir"
	case HostKeyPassphrase:
		return "passphrase-wrapped"
	case HostKeySystemdCredential:
		if TPMPresent() {
			return "systemd-credential (TPM present)"
		}
		return "systemd-credential"
	default:
		return "absent"
	}
}

// Secure reports whether a stolen data volume alone is insufficient to recover
// the host key under this protection.
func (p HostKeyProtection) Secure() bool {
	return p == HostKeyPassphrase || p == HostKeySystemdCredential
}

// TPMPresent reports whether a TPM 2.0 resource manager is available, so the
// systemd-credential tier can be described as TPM-backed only when it truly is.
func TPMPresent() bool {
	_, err := os.Stat(tpmResourceMgr)
	return err == nil
}

// credentialHostKeyPath returns the host.key path inside systemd's credential
// directory, and whether that file exists.
func credentialHostKeyPath() (string, bool) {
	credDir := os.Getenv(EnvCredentialsDirectory)
	if credDir == "" {
		return "", false
	}
	path := filepath.Join(credDir, hostKeyFile)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

// hostKeyPassphrase resolves the operator passphrase from the environment or a
// file. The bool reports whether one was configured at all.
func hostKeyPassphrase() (string, bool, error) {
	if p := os.Getenv(EnvHostKeyPassphrase); p != "" {
		return p, true, nil
	}
	if path := os.Getenv(EnvHostKeyPassphraseFile); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return "", true, fmt.Errorf("secrets: read %s=%s: %w", EnvHostKeyPassphraseFile, path, err)
		}
		p := strings.TrimRight(string(body), "\r\n")
		if p == "" {
			return "", true, fmt.Errorf("secrets: %s=%s is empty", EnvHostKeyPassphraseFile, path)
		}
		return p, true, nil
	}
	return "", false, nil
}

// InspectHostKeyProtection reports how the host key in dataDir is protected at
// rest, without unwrapping the DEK. It is what init and doctor read to state the
// real posture.
func InspectHostKeyProtection(dataDir string) HostKeyProtection {
	if _, ok := credentialHostKeyPath(); ok {
		return HostKeySystemdCredential
	}
	if _, err := os.Stat(filepath.Join(dataDir, hostKeyAgeFile)); err == nil {
		return HostKeyPassphrase
	}
	if _, err := os.Stat(filepath.Join(dataDir, hostKeyFile)); err == nil {
		return HostKeyPlaintext
	}
	return HostKeyAbsent
}

// loadHostIdentity resolves the host identity by precedence — systemd
// credential, then passphrase-wrapped, then plaintext — and reports which tier
// answered so the envelope can carry the real protection state.
func loadHostIdentity(dataDir string) (*age.X25519Identity, HostKeyProtection, error) {
	if path, ok := credentialHostKeyPath(); ok {
		id, err := parseHostKeyFile(path)
		if err != nil {
			return nil, HostKeyAbsent, err
		}
		return id, HostKeySystemdCredential, nil
	}

	wrapped := filepath.Join(dataDir, hostKeyAgeFile)
	if _, err := os.Stat(wrapped); err == nil {
		passphrase, ok, perr := hostKeyPassphrase()
		if perr != nil {
			return nil, HostKeyAbsent, perr
		}
		if !ok {
			return nil, HostKeyAbsent, fmt.Errorf(
				"secrets: %s holds a passphrase-wrapped host key but no passphrase is set "+
					"(%s or %s)", hostKeyAgeFile, EnvHostKeyPassphrase, EnvHostKeyPassphraseFile)
		}
		id, err := decryptWrappedHostKey(wrapped, passphrase)
		if err != nil {
			return nil, HostKeyAbsent, err
		}
		return id, HostKeyPassphrase, nil
	}

	plain := filepath.Join(dataDir, hostKeyFile)
	id, err := parseHostKeyFile(plain)
	if err != nil {
		return nil, HostKeyAbsent, err
	}
	return id, HostKeyPlaintext, nil
}

func parseHostKeyFile(path string) (*age.X25519Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: read host key: %w", err)
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("secrets: parse host key: %w", err)
	}
	return id, nil
}

// writeWrappedHostKey scrypt-wraps the host identity under passphrase and writes
// it to path (armored age), 0400. Nothing plaintext touches the data dir.
func writeWrappedHostKey(path string, id *age.X25519Identity, passphrase string) error {
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return fmt.Errorf("secrets: scrypt recipient: %w", err)
	}
	var buf bytes.Buffer
	armored := armor.NewWriter(&buf)
	w, err := age.Encrypt(armored, r)
	if err != nil {
		return fmt.Errorf("secrets: wrap host key: %w", err)
	}
	if _, err := io.WriteString(w, id.String()+"\n"); err != nil {
		return fmt.Errorf("secrets: wrap host key: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("secrets: wrap host key: %w", err)
	}
	if err := armored.Close(); err != nil {
		return fmt.Errorf("secrets: armor host key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fmt.Errorf("secrets: create wrapped host key: %w", err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return fmt.Errorf("secrets: write wrapped host key: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("secrets: write wrapped host key: %w", err)
	}
	return nil
}

func decryptWrappedHostKey(path, passphrase string) (*age.X25519Identity, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: read wrapped host key: %w", err)
	}
	scrypt, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("secrets: scrypt identity: %w", err)
	}
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(body)), scrypt)
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap host key (wrong passphrase?): %w", err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap host key: %w", err)
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("secrets: parse unwrapped host key: %w", err)
	}
	return id, nil
}
