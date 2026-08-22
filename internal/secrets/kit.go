package secrets

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Encoding is the form a consumer wants the 32 derived bytes in.
type Encoding string

// The two forms any consumer in this system takes: a password it types into a
// tool, or raw key material printed as hex.
const (
	EncodingBase64 Encoding = "base64"
	EncodingHex    Encoding = "hex"
)

// Purpose is one HKDF info-string family: what it feeds, what its id names,
// and how the derived bytes reach that consumer. The card's section 5 and
// `exocomp kit derive` render from this one table, so a purpose cannot be
// printed on a card and be unknown to the CLI.
type Purpose struct {
	Name     string
	Alias    string
	IDLabel  string
	Consumer string
	Encoding Encoding
}

var purposes = []Purpose{
	{"restic-repo", "restic", "<target-id>", "restic repository password", EncodingBase64},
	{"tofu-state", "tofu", "<workspace-id>", "OpenTofu state encryption passphrase", EncodingBase64},
	{"ansible-vault", "", "<project-id>", "ansible-vault password", EncodingBase64},
	{"tf-backend", "backend", "<workspace-id>", "state backend bearer token", EncodingBase64},
	{"backup-manifest-mac", "", "<target-id>", "backup manifest HMAC key", EncodingHex},
	{"webhook-hmac", "", "<source-id>", "webhook signature key", EncodingHex},
	{"approval-token", "", "<id>", "approval token signing key", EncodingHex},
}

// Purposes lists every derivation family in use, in card order.
func Purposes() []Purpose { return append([]Purpose(nil), purposes...) }

// LookupPurpose resolves a purpose by its name or by the short alias the card
// prints, so `kit derive restic/nas` and `kit derive restic-repo/nas` are the
// same derivation.
func LookupPurpose(name string) (Purpose, bool) {
	for _, p := range purposes {
		if name == p.Name || (p.Alias != "" && name == p.Alias) {
			return p, true
		}
	}
	return Purpose{}, false
}

// Encode renders derived bytes the way this purpose's consumer takes them.
func (p Purpose) Encode(key []byte) string {
	if p.Encoding == EncodingHex {
		return hex.EncodeToString(key)
	}
	return base64.StdEncoding.EncodeToString(key)
}

// KitTarget is one backup target as the card names it. The kit renderer takes
// them from the caller: this package holds no store handle.
type KitTarget struct {
	Name string
	Kind string
	URI  string
}

// RecoveryKit is the printed card. AgeSecretKey is populated only when the
// recovery identity is minted; it is never written to disk or to the database.
type RecoveryKit struct {
	AgeSecretKey string
	Version      KeyringVersion
	// Host names the machine the kit was minted on. Empty omits it.
	Host       string
	Salt       string
	Recipients []Recipient
	Targets    []KitTarget
}

// VersionLine is the card's identity line: which generation this page is, when
// it was minted, and — from the second generation on — that the page it
// replaces is now waste paper.
func (k *RecoveryKit) VersionLine() string {
	line := "kit " + k.Version.String()
	if k.Host != "" {
		line = k.Host + " · " + line
	}
	if !k.Version.Generated.IsZero() {
		line += " · generated " + k.Version.Generated.UTC().Format("2006-01-02")
	}
	if k.Version.Version > firstKeyringVersion {
		line += " · replaces v" + strconv.Itoa(k.Version.Version-1) + " (destroy it)"
	}
	return line
}

// digestPrefix is the line carrying the page's own sha256. It is stripped
// before the digest is computed, so a printed page hashes to what it prints.
const digestPrefix = "kit sha256 · "

// Render returns the card text: numbered sections, then the page digest.
func (k *RecoveryKit) Render() string {
	body := k.renderBody()
	return body + digestPrefix + groupHex(PageSHA256(body), 8) +
		" · exocomp kit verify checks this page against the daemon\n"
}

// PageSHA256 is the digest of a card with its own digest line removed. A
// printed page and the daemon's current rendering therefore hash to the same
// value exactly when they say the same thing.
func PageSHA256(page string) string {
	var kept strings.Builder
	for line := range strings.Lines(page) {
		if strings.HasPrefix(line, digestPrefix) {
			continue
		}
		kept.WriteString(line)
	}
	sum := sha256.Sum256([]byte(kept.String()))
	return hex.EncodeToString(sum[:])
}

// ParsePageSHA256 returns the digest a rendered page prints, in the grouped
// form it prints it in.
func ParsePageSHA256(page string) (string, bool) {
	for line := range strings.Lines(page) {
		rest, ok := strings.CutPrefix(line, digestPrefix)
		if !ok {
			continue
		}
		printed, _, _ := strings.Cut(rest, " · ")
		return strings.TrimSpace(printed), true
	}
	return "", false
}

// groupHex renders the first n bytes of a hex digest in four-character groups,
// the way an operator compares two of them by eye. Like a recipient
// fingerprint, the truncation identifies a page; `kit verify` compares the
// whole digest.
func groupHex(digest string, n int) string {
	if len(digest) > 2*n {
		digest = digest[:2*n]
	}
	groups := make([]string, 0, len(digest)/4)
	for i := 0; i+4 <= len(digest); i += 4 {
		groups = append(groups, digest[i:i+4])
	}
	return strings.Join(groups, " ")
}

func (k *RecoveryKit) renderBody() string {
	lines := []string{
		"exocomp · RECOVERY KIT",
		k.VersionLine(),
		"FIRE SAFE ONLY. Verify quarterly: exocomp kit verify",
		strings.Repeat("=", 72),
		"",
		"This page regenerates every secret exocomp manages. With it - and any one",
		"backup target in section 6 - the whole system can be rebuilt on new",
		"hardware. Commands are verbatim: type them exactly as printed.",
		"",
	}
	lines = append(lines, k.sectionKey()...)
	lines = append(lines, k.sectionRecipients()...)
	lines = append(lines, k.sectionSalt()...)
	lines = append(lines, sectionStorageCredential()...)
	lines = append(lines, k.sectionDerivation()...)
	lines = append(lines, k.sectionTargets()...)
	return strings.Join(lines, "\n") + "\n"
}

func (k *RecoveryKit) sectionKey() []string {
	lines := []string{"1 · RECOVERY KEY (SECRET - THIS IS THE ROOT)", ""}
	if k.AgeSecretKey != "" {
		return append(lines,
			"Shown once, here, and never again. Store it offline now: it is not on",
			"disk and not in the database.",
			"",
			"    "+k.AgeSecretKey,
			"",
		)
	}
	return append(lines,
		"The recovery age secret key is NOT shown here.",
		"It appears only when it is minted: at `exocomp init`, or when the",
		"recovery identity is rotated. If you no longer have it, re-key - that",
		"mints a new recovery identity and revokes the old keyring row.",
		"",
	)
}

func (k *RecoveryKit) sectionRecipients() []string {
	lines := []string{
		"2 · RECIPIENTS (PUBLIC - FOR VERIFICATION)",
		"",
		"Any one of them unwraps the DEK.",
		"",
	}
	for _, r := range k.Recipients {
		lines = append(lines, fmt.Sprintf("    %-10s %s  %s", r.Label, r.Fingerprint, r.Public))
	}
	return append(lines, "")
}

func (k *RecoveryKit) sectionSalt() []string {
	return []string{
		"3 · SALT (PUBLIC - NEEDED FOR EVERY DERIVATION)",
		"",
		"    " + k.Salt,
		"",
	}
}

func sectionStorageCredential() []string {
	return []string{
		"4 · READ-ONLY STORAGE CREDENTIAL (WRITE BY HAND)",
		"",
		"exocomp prints no storage credential. A read-only key for the target you",
		"would restore from is yours to create and write here, in ink.",
		"",
		"    +----------------------------------------------------------------+",
		"    | target uri:                                                    |",
		"    | key id:                                                        |",
		"    | secret:                                                        |",
		"    +----------------------------------------------------------------+",
		"",
	}
}

// sectionDerivation renders the derivation block from the implementation: the
// salt constant, the info-string rule, and the purpose table. kit_test.go
// executes the openssl pipeline below and diffs the result against Derive,
// because a card whose commands do not run is worse than no card.
func (k *RecoveryKit) sectionDerivation() []string {
	lines := []string{
		"5 · DERIVATION RECIPE (GENERAL - ONE RULE)",
		"",
		"The keyring wraps one 32-byte data encryption key (the DEK). Nothing else",
		"is stored: every consumer's key is HKDF-SHA256 over the DEK with the salt",
		`in section 3 and an info string of "<purpose>:<id>", 32 bytes out.`,
		"",
	}
	for _, p := range purposes {
		lines = append(lines, fmt.Sprintf("    %-32s %s, %s",
			p.Name+":"+p.IDLabel, p.Consumer, p.Encoding))
	}
	lines = append(lines,
		"",
		"With the daemon up:",
		"",
		"    exocomp kit derive restic/<target-id>",
		"    exocomp kit derive ansible-vault/<project-id>",
		"    exocomp kit derive --list      # every purpose:id in use",
		"",
		"By hand, with only age, openssl, restic and coreutils - no exocomp binary",
		"anywhere on this path:",
		"",
		"  1. Fetch keyring.age from any backup target root. It sits outside the",
		"     restic repository, because the repository password is derived from",
		"     the key it wraps.",
		"",
		"  2. Unwrap the DEK with the recovery key in section 1:",
		"",
		"       age -d -i recovery.key keyring.age > dek.bin",
		"",
		"  3. Derive the key for one purpose:id - here the restic password for the",
		"     target you are restoring from:",
		"",
		`       hex() { od -An -tx1 -v | tr -d ' \n'; }`,
		"       openssl kdf -keylen 32 -kdfopt digest:SHA256 \\",
		`         -kdfopt "hexkey:$(hex < dek.bin)" \`,
		fmt.Sprintf(`         -kdfopt "hexsalt:$(printf '%s' | hex)" \`, k.Salt),
		`         -kdfopt "hexinfo:$(printf 'restic-repo:<target-id>' | hex)" \`,
		"         -binary HKDF | base64 > restic.pass",
		"",
		"  4. Restore. Section 4 carries the storage credential, if the target",
		"     needs one; nothing is needed for a local path or a mounted share.",
		"     The RECOVERY.md beside keyring.age names the uri this target was",
		"     written to.",
		"",
		"       RESTIC_PASSWORD_FILE=restic.pass restic -r <target-uri> \\",
		"         restore latest --target /srv/exocomp",
		"",
		"  5. Start the service and run: exocomp doctor",
		"",
	)
	return lines
}

func (k *RecoveryKit) sectionTargets() []string {
	lines := []string{"6 · TARGETS (WHERE THE DATA IS)", ""}
	if len(k.Targets) == 0 {
		return append(lines,
			"No backup target is recorded yet. Add one with `exocomp backup add`",
			"and reprint this page.",
			"",
		)
	}
	for _, t := range k.Targets {
		lines = append(lines, fmt.Sprintf("    %-16s %-13s %s", t.Name, t.Kind, t.URI))
	}
	return append(lines, "")
}
