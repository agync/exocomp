package main

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestCompleteRedactor: the web surface's redactor is built only from a
// complete secret set. A secret that opens is scrubbed; one that will not open
// makes the build fail, so the surface drops command text instead of scrubbing
// partially.
func TestCompleteRedactor(t *testing.T) {
	st, env := ntfyTestStore(t)
	ctx := context.Background()
	sealNtfy(t, st, env, "svc/token", "tok-complete-set-value")

	red, err := completeRedactor(ctx, st, env)
	if err != nil {
		t.Fatalf("complete set: %v", err)
	}
	if got := string(red.Redact([]byte("auth tok-complete-set-value end"))); got != "auth [REDACTED] end" {
		t.Errorf("Redact = %q", got)
	}

	if _, err := st.CreateSecret(ctx, store.SecretRow{
		Name: "svc/broken", Kind: "generic", Nonce: []byte("short"), Ciphertext: []byte("not a sealed value"),
		AAD: "svc/broken|generic",
	}); err != nil {
		t.Fatalf("create broken secret: %v", err)
	}
	if red, err := completeRedactor(ctx, st, env); err == nil || red != nil {
		t.Errorf("incomplete set: got redactor %v, err %v; want a refusal", red, err)
	}
}
