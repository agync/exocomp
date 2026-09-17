package backup

import (
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestRenderRecoveryImageRef: the recovery notes name the image this
// deployment runs, because a local tag dies with the host. An empty ref must
// omit the line entirely rather than print an empty value an operator could
// read as "nothing to load".
func TestRenderRecoveryImageRef(t *testing.T) {
	t.Parallel()

	tgt := Target{BackupTarget: store.BackupTarget{
		Name: "nas", Kind: KindResticLocal, URI: "/srv/exocomp",
	}}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	withFacts := factsBlock(t, string(renderRecovery(tgt, nil, now, "ghcr.io/abn/exocomp:latest")))
	t.Logf("Facts with a ref:\n%s", withFacts)
	if !strings.Contains(withFacts, "image-ref: ghcr.io/abn/exocomp:latest\n") {
		t.Errorf("Facts block does not carry the image ref:\n%s", withFacts)
	}

	without := string(renderRecovery(tgt, nil, now, ""))
	t.Logf("Facts without a ref:\n%s", factsBlock(t, without))
	if strings.Contains(without, "image-ref:") {
		t.Errorf("an empty ref still produced an image-ref line:\n%s", without)
	}
}

// factsBlock returns the text between the Facts fence, so an assertion cannot
// accidentally match an image-ref mention elsewhere in the notes.
func factsBlock(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "## Facts\n\n```\n")
	if !ok {
		t.Fatalf("recovery notes have no Facts block:\n%s", body)
	}
	facts, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatalf("Facts block is unterminated:\n%s", body)
	}
	return facts
}
