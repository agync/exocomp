package packaging

import (
	"strings"
	"testing"
)

// TestComposeDocDocumentsThePassphrasePrerequisites: the passphrase file's mode
// and relabel are requirements the container cannot enforce, so the doc is the
// contract. A reader has to find the mounted path, the 0644 mode, and the
// Podman-only keep-id alternative that keeps a 0600 file private.
func TestComposeDocDocumentsThePassphrasePrerequisites(t *testing.T) {
	body := readPackagingFile(t, "../docs/operations/containers.md")

	for _, want := range []string{
		"/run/secrets/host-key.passphrase",
		"chmod 644",
		"userns_mode: keep-id",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("containers.md does not name %q; the compose passphrase prerequisite is undocumented", want)
		}
	}
}
