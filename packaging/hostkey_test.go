package packaging

import (
	"strings"
	"testing"
)

// TestQuadletLoadsTheHostKeyCredential: the primary systemd tier must deliver
// host.key through an encrypted systemd credential, so a stolen data volume
// does not contain a plaintext key that unwraps the DEK. The
// unwrap secret lives outside the volume by construction of systemd-creds.
func TestQuadletLoadsTheHostKeyCredential(t *testing.T) {
	body := readPackagingFile(t, "quadlet/exocomp.container")

	if !strings.Contains(body, "LoadCredentialEncrypted=host.key:") {
		t.Error("the quadlet does not load host.key as an encrypted systemd credential; " +
			"the default host-key-at-rest story is not secure")
	}
}

// TestBootstrapDocWrapsTheHostKey: the host-key-at-rest operation is documented
// rather than scripted — an operator moves the key off the data volume into a
// systemd credential, preferring TPM binding when a TPM is present, and deletes
// the plaintext afterwards. The doc in docs/operations/bootstrap.md is the
// contract, so it must carry the whole command.
func TestBootstrapDocWrapsTheHostKey(t *testing.T) {
	body := readPackagingFile(t, "../docs/operations/bootstrap.md")

	for _, want := range []string{
		"systemd-creds encrypt",
		"--with-key=host+tpm2",
		"--with-key=host",
		"host.key.cred",
		"/dev/tpmrm0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bootstrap.md does not %q; the host-key credential path is incomplete", want)
		}
	}
	if !strings.Contains(body, "rm -f") || !strings.Contains(body, "host.key") {
		t.Error("bootstrap.md does not remove the plaintext host.key after wrapping it")
	}
}
