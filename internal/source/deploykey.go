package source

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// DeployKey is a freshly generated ed25519 keypair for a git source. The
// private half is returned as OpenSSH PEM for the caller to seal into the
// envelope; the public half is the one-line entry the operator adds to the
// forge as a read-only deploy key. The private bytes never touch disk in this
// package — the caller seals them, and Sync writes them to a 0700 file only for
// the length of one git command.
type DeployKey struct {
	// PrivatePEM is the unencrypted OpenSSH private key. It is a secret: seal it
	// immediately and never log or persist it in the clear.
	PrivatePEM []byte
	// PublicAuthorizedKey is the "ssh-ed25519 AAAA... exocomp-<name>" line for
	// the forge. It carries no secret.
	PublicAuthorizedKey string
}

// GenerateDeployKey makes a new ed25519 deploy key labelled for the source. The
// key is unencrypted because it lives sealed in the envelope, not on disk:
// wrapping it in a passphrase would only move the secret, and the envelope is
// where this project's secret-at-rest story already lives.
func GenerateDeployKey(sourceName string) (DeployKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return DeployKey{}, fmt.Errorf("source: generate deploy key: %w", err)
	}

	comment := "exocomp-" + strings.TrimSpace(sourceName)
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return DeployKey{}, fmt.Errorf("source: marshal deploy key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return DeployKey{}, fmt.Errorf("source: deploy public key: %w", err)
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))

	return DeployKey{
		PrivatePEM:          pem.EncodeToMemory(block),
		PublicAuthorizedKey: authorized + " " + comment,
	}, nil
}

// PublicFromPrivatePEM derives the "ssh-ed25519 AAAA... <comment>" authorized-key
// line from an OpenSSH private key PEM, so a caller holding only the sealed
// private half (the fleet key) can print the public one to authorize on a host.
// comment, when non-empty, is appended so the line stays recognizable.
func PublicFromPrivatePEM(privatePEM []byte, comment string) (string, error) {
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return "", fmt.Errorf("source: parse private key: %w", err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment = strings.TrimSpace(comment); comment != "" {
		line += " " + comment
	}
	return line, nil
}
