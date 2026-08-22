package web

import "testing"

func TestSecretHashVerify(t *testing.T) {
	secret, err := newSecret()
	if err != nil {
		t.Fatalf("newSecret: %v", err)
	}
	hash, err := hashSecret(secret)
	if err != nil {
		t.Fatalf("hashSecret: %v", err)
	}
	if hash == secret {
		t.Fatal("the stored hash must not be the secret")
	}

	ok, err := verifySecret(secret, hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("the minted secret must verify against its own hash")
	}

	ok, err = verifySecret(secret+"x", hash)
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Fatal("a wrong secret must not verify")
	}
}

func TestHashSaltIsFresh(t *testing.T) {
	// Two hashes of the same secret must differ: a fresh salt each time.
	h1, _ := hashSecret("same")
	h2, _ := hashSecret("same")
	if h1 == h2 {
		t.Fatal("hashes of the same secret must differ (per-hash salt)")
	}
	if ok, _ := verifySecret("same", h1); !ok {
		t.Fatal("h1 must still verify")
	}
	if ok, _ := verifySecret("same", h2); !ok {
		t.Fatal("h2 must still verify")
	}
}

func TestTokenFormatRoundTrip(t *testing.T) {
	token := formatToken(42, "deadbeef")
	id, secret, err := parseToken(token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if id != 42 || secret != "deadbeef" {
		t.Fatalf("round-trip = (%d, %q), want (42, deadbeef)", id, secret)
	}
}

func TestParseTokenRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "nope", "exo_", "exo_x_secret", "exo_1_", "other_1_secret", "exo_-1_s"} {
		if _, _, err := parseToken(bad); err == nil {
			t.Fatalf("parseToken(%q) accepted a malformed token", bad)
		}
	}
}

func TestVerifyRejectsBadPHC(t *testing.T) {
	if _, err := verifySecret("x", "not-a-phc-string"); err == nil {
		t.Fatal("a malformed stored hash must be an error, never a silent pass")
	}
}
