package main

import (
	"fmt"
	"os"
	"path/filepath"

	"exocomp.dev/exocomp/internal/secrets"
)

// sensitiveArtifact reports whether an artifact of this kind is encrypted at
// rest. A saved OpenTofu plan is: it carries every attribute of every resource
// it would change, in the clear, which the approval view renders and the
// database never stores. The list is a policy, not a schema — the kind on the
// row decides, so an artifact that outlives its row is still readable by name.
func sensitiveArtifact(kind string) bool {
	return kind == planArtifactKind
}

// sealArtifact encrypts a sensitive artifact to the keyring's recipients and
// returns the path it now lives at. A kind that is not sensitive keeps its
// path.
func sealArtifact(env *secrets.Envelope, kind, path string) (string, error) {
	if env == nil || !sensitiveArtifact(kind) {
		return path, nil
	}
	sealed, err := env.SealFile(path)
	if err != nil {
		return "", fmt.Errorf("workspace: seal %s artifact: %w", kind, err)
	}
	return sealed, nil
}

// openPlanForRun writes a saved plan out in the clear for the one run that
// consumes it, under the 0700 private dir, and returns the path with the
// function that removes it. tofu takes a file, not a stream, so the plaintext
// has to exist — for the length of the apply and no longer.
func openPlanForRun(env *secrets.Envelope, privateRoot, path string) (string, func(), error) {
	noop := func() {}
	if env == nil || filepath.Ext(path) != secrets.SealedSuffix {
		return path, noop, nil
	}

	plaintext, err := env.OpenSealedFile(path)
	if err != nil {
		return "", noop, fmt.Errorf("workspace apply: %w", err)
	}
	dir, err := os.MkdirTemp(privateRoot, "plan-")
	if err != nil {
		return "", noop, fmt.Errorf("workspace apply: private dir: %w", err)
	}
	out := filepath.Join(dir, "saved.tfplan")
	if err := os.WriteFile(out, plaintext, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", noop, fmt.Errorf("workspace apply: write plan: %w", err)
	}
	clear(plaintext)
	return out, func() { _ = os.RemoveAll(dir) }, nil
}
