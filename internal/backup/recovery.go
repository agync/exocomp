package backup

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
)

// renderRecovery writes the target-specific half of the recovery card: the
// facts a restoring operator needs about THIS target, and the commands that
// get from the printed recovery key to a restored data dir using only age,
// openssl and restic. The printed kit carries the key; this file carries
// everything else, which is why it is safe to leave at the target root.
func renderRecovery(t Target, recipients []secrets.Recipient, now time.Time) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# exocomp recovery notes for target %q\n\n", t.Name)
	b.WriteString("`keyring.age` sits beside this file, outside the backup repository, because\n" +
		"the repository password is derived from the key it wraps. It is ciphertext:\n" +
		"only a keyring recipient below can unwrap it.\n\n")

	b.WriteString("## Facts\n\n```\n")
	fmt.Fprintf(&b, "target-name: %s\n", t.Name)
	fmt.Fprintf(&b, "target-kind: %s\n", t.Kind)
	fmt.Fprintf(&b, "target-uri: %s\n", t.URI)
	fmt.Fprintf(&b, "hkdf-salt: %s\n", secrets.Salt)
	if t.Kind != KindGit {
		fmt.Fprintf(&b, "hkdf-info: restic-repo:%s\n", t.Name)
	}
	fmt.Fprintf(&b, "written-at: %s\n", now.Format(time.RFC3339))
	b.WriteString("```\n\n")

	b.WriteString("## Recover by hand\n\n")
	b.WriteString("```sh\n")
	b.WriteString("age -d -i recovery.key keyring.age > dek.bin\n")
	if t.Kind == KindGit {
		fmt.Fprintf(&b, "git clone --branch backup %s snapshots\n", t.URI)
		b.WriteString("age -d -i recovery.key snapshots/snapshot-*.age > snapshot.tar.zst\n")
		b.WriteString("zstd -d -c snapshot.tar.zst | tar -x -C /srv/exocomp\n")
		b.WriteString("# gzip archives: gzip -dc snapshot.tar.gz | tar -x -C /srv/exocomp\n")
	} else {
		b.WriteString("hex() { od -An -tx1 -v | tr -d ' \\n'; }\n")
		b.WriteString("openssl kdf -keylen 32 -kdfopt digest:SHA256 \\\n")
		b.WriteString("  -kdfopt \"hexkey:$(hex < dek.bin)\" \\\n")
		fmt.Fprintf(&b, "  -kdfopt \"hexsalt:$(printf '%s' | hex)\" \\\n", secrets.Salt)
		fmt.Fprintf(&b, "  -kdfopt \"hexinfo:$(printf 'restic-repo:%s' | hex)\" \\\n", t.Name)
		b.WriteString("  -binary HKDF | base64 > restic.pass\n")
		fmt.Fprintf(&b, "RESTIC_PASSWORD_FILE=restic.pass restic -r %s \\\n", t.URI)
		b.WriteString("  restore latest --target /srv/exocomp\n")
	}
	b.WriteString("```\n\n")
	b.WriteString("The restored tree holds `" + SnapshotName + "` (plain SQLite; rename it to `" +
		RestoredDBName + "`),\n`" + KeyringName + "`, and `" + ManifestName +
		"` with the sha256 of both.\n\n")

	b.WriteString("## Keyring recipients\n\n")
	for _, r := range recipients {
		fmt.Fprintf(&b, "- %s  %s  %s\n", r.Label, r.Fingerprint, r.Public)
	}
	return []byte(b.String())
}

// targetNameFromRecovery reads the target name back out of a RECOVERY.md, so a
// restore can derive the repository password without the operator having to
// remember which name the target was created under.
func targetNameFromRecovery(body []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		name, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "target-name:")
		if ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}
