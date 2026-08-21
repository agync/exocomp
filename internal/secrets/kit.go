package secrets

import (
	"strconv"
	"strings"
)

// derivationRecipe is the printed card's manual path. Its literal text is a
// build artifact: kit_test.go executes the openssl pipeline below and diffs the
// result against Derive, because a card whose commands do not run is worse than
// no card.
const derivationRecipe = `How every other secret is derived
---------------------------------

The keyring wraps one 32-byte data encryption key (the DEK). Nothing else is
stored: each consumer's key is derived from the DEK with HKDF-SHA256, using the
salt above and an info string of "<purpose>:<id>". The output is 32 bytes.

  restic-repo:<target-id>        restic repository password, base64 of the 32 bytes
  tofu-state:<workspace-id>      OpenTofu state encryption passphrase
  ansible-vault:<project-id>     ansible-vault password
  webhook-hmac:<source-id>       webhook signature key
  approval-token:<id>            approval token signing key

Recovering by hand, with only age, openssl, restic and coreutils - no exocomp
binary anywhere on this path:

  1. Fetch keyring.age from any backup target root. It sits outside the restic
     repository, because the repository password is derived from the key it
     wraps.

  2. Unwrap the DEK with the recovery key printed on this card:

       age -d -i recovery.key keyring.age > dek.bin

  3. Derive the restic password for the target you are restoring from:

       hex() { od -An -tx1 -v | tr -d ' \n'; }
       openssl kdf -keylen 32 -kdfopt digest:SHA256 \
         -kdfopt "hexkey:$(hex < dek.bin)" \
         -kdfopt "hexsalt:$(printf 'exocomp/v1' | hex)" \
         -kdfopt "hexinfo:$(printf 'restic-repo:<target-id>' | hex)" \
         -binary HKDF | base64 > restic.pass

  4. Restore. This card carries no storage credential: you supply
     <target-uri> and whatever reaching it needs - nothing for a local path or
     a mounted share, AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for S3. The
     RECOVERY.md beside keyring.age names the uri this target was written to.
     Per-target read-only credentials arrive on this card in M2.

       RESTIC_PASSWORD_FILE=restic.pass restic -r <target-uri> \
         restore latest --target /srv/exocomp

  5. Start the service and run: exocomp doctor`

// RecoveryKit is the printed card. AgeSecretKey is populated only when the
// recovery identity is minted; it is never written to disk or to the database.
type RecoveryKit struct {
	AgeSecretKey string
	Version      KeyringVersion
	Salt         string
	Derivation   string
	Recipients   []Recipient
}

// VersionLine is the card's identity line: which generation this page is, when
// it was minted, and — from the second generation on — that the page it
// replaces is now waste paper.
func (k *RecoveryKit) VersionLine() string {
	line := "kit " + k.Version.String()
	if !k.Version.Generated.IsZero() {
		line += " · generated " + k.Version.Generated.UTC().Format("2006-01-02")
	}
	if k.Version.Version > firstKeyringVersion {
		line += " · replaces v" + strconv.Itoa(k.Version.Version-1) + " (destroy it)"
	}
	return line
}

// Render returns the card text. Without AgeSecretKey it renders the structure
// only and says so.
func (k *RecoveryKit) Render() string {
	lines := []string{
		"exocomp recovery kit",
		"====================",
		k.VersionLine(),
		"",
	}

	if k.AgeSecretKey != "" {
		lines = append(lines,
			"RECOVERY AGE SECRET KEY - shown once, here, and never again.",
			"Store it offline now. It is not on disk and not in the database.",
			"",
			"    "+k.AgeSecretKey,
			"",
		)
	} else {
		lines = append(lines,
			"The recovery age secret key is NOT shown here.",
			"It appears only when it is minted: at `exocomp init`, or when the",
			"recovery identity is rotated. If you no longer have it, re-key - that",
			"mints a new recovery identity and revokes the old keyring row.",
			"",
		)
	}

	lines = append(lines, "Keyring recipients - any one of them unwraps the DEK:", "")
	for _, r := range k.Recipients {
		lines = append(lines, "    "+r.Label+"  "+r.Fingerprint+"  "+r.Public)
	}

	lines = append(lines,
		"",
		"HKDF salt: "+k.Salt,
		"",
		k.Derivation,
	)
	return strings.Join(lines, "\n") + "\n"
}
