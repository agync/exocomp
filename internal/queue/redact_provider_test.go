package queue_test

import (
	"context"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
)

// A secret learned after the queue was built must still be scrubbed: the
// provider resolves at claim time, not at process start.
func TestRedactorProviderResolvesPerJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const secret = "leak-alpha-value-nowhere-else"
	st := newStore(t)
	var known [][]byte
	q := queue.New(st, leakyDriver{secret: secret}, quietOptions(),
		queue.WithRedactorProvider(func() *secrets.Redactor {
			cp := make([][]byte, len(known))
			copy(cp, known)
			return secrets.NewRedactor(cp)
		}))

	run := func() int64 {
		job, err := q.Enqueue(ctx, queue.Spec{
			Runbook: "test", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		drain(t, q)
		return job.ID
	}

	first := run()
	known = append(known, []byte(secret))
	second := run()

	if !strings.Contains(eventText(t, st, first), secret) {
		t.Fatalf("pre-seal job should carry the plaintext: the redactor could not know it yet")
	}
	post := eventText(t, st, second)
	if strings.Contains(post, secret) {
		t.Errorf("post-seal job leaked the plaintext despite the provider:\n%s", post)
	}
	if !strings.Contains(post, secrets.Marker) {
		t.Errorf("post-seal job shows no redaction marker:\n%s", post)
	}
}
