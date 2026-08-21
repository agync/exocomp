package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	mrand "math/rand/v2"
	"strings"
	"testing"
)

func encodings(secret []byte) [][]byte {
	lower := hex.EncodeToString(secret)
	return [][]byte{
		bytes.Clone(secret),
		[]byte(base64.StdEncoding.EncodeToString(secret)),
		[]byte(lower),
		[]byte(strings.ToUpper(lower)),
	}
}

func writeChunked(t *testing.T, r *Redactor, stream []byte, rng *mrand.Rand) []byte {
	t.Helper()
	var out bytes.Buffer
	w := r.Wrap(&out)
	for off := 0; off < len(stream); {
		n := min(1+rng.IntN(17), len(stream)-off)
		if _, err := w.Write(stream[off : off+n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		off += n
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes()
}

// TestRedactorScrubsEmbeddedSecrets embeds each known secret raw, base64-encoded
// and hex-encoded at random offsets in a noisy stream, feeds it through the
// writer in random-sized chunks, and asserts the output holds no occurrence of
// any encoding and matches the model byte for byte.
func TestRedactorScrubsEmbeddedSecrets(t *testing.T) {
	for round := range 200 {
		rng := mrand.New(mrand.NewPCG(0x9E3779B97F4A7C15, uint64(round)))

		secrets := make([][]byte, 1+rng.IntN(3))
		for i := range secrets {
			secrets[i] = make([]byte, 16+rng.IntN(17))
			if _, err := rand.Read(secrets[i]); err != nil {
				t.Fatalf("rand: %v", err)
			}
		}

		noise := func() []byte {
			b := make([]byte, rng.IntN(64))
			for i := range b {
				b[i] = byte(' ' + rng.IntN(0x5f))
			}
			return b
		}

		var stream, want bytes.Buffer
		for range 1 + rng.IntN(12) {
			n := noise()
			stream.Write(n)
			want.Write(n)

			enc := encodings(secrets[rng.IntN(len(secrets))])[rng.IntN(4)]
			stream.Write(enc)
			want.WriteString(Marker)
		}
		tail := noise()
		stream.Write(tail)
		want.Write(tail)

		got := writeChunked(t, NewRedactor(secrets), stream.Bytes(), rng)

		for i, s := range secrets {
			for j, enc := range encodings(s) {
				if bytes.Contains(got, enc) {
					t.Fatalf("round %d: secret %d encoding %d survived redaction", round, i, j)
				}
			}
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("round %d: output differs from the model\n got %q\nwant %q",
				round, got, want.Bytes())
		}
	}
}

// TestRedactorHandlesEverySplitPoint drives the boundary case directly: a
// secret cut in two at each possible offset across two Writes.
func TestRedactorHandlesEverySplitPoint(t *testing.T) {
	secret := []byte("AGE-SECRET-KEY-1EXAMPLEEXAMPLEEXAMPLE")
	r := NewRedactor([][]byte{secret})

	stream := append(append([]byte("before "), secret...), " after"...)
	for split := range len(stream) + 1 {
		var out bytes.Buffer
		w := r.Wrap(&out)
		if _, err := w.Write(stream[:split]); err != nil {
			t.Fatalf("split %d: first write: %v", split, err)
		}
		if _, err := w.Write(stream[split:]); err != nil {
			t.Fatalf("split %d: second write: %v", split, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("split %d: close: %v", split, err)
		}
		if want := "before " + Marker + " after"; out.String() != want {
			t.Fatalf("split %d: got %q, want %q", split, out.String(), want)
		}
	}
}

func TestRedactorSingleByteWrites(t *testing.T) {
	secret := []byte("swordfish-swordfish")
	var out bytes.Buffer
	w := NewRedactor([][]byte{secret}).Wrap(&out)

	stream := []byte("a" + string(secret) + "b" + string(secret))
	for i := range stream {
		if _, err := w.Write(stream[i : i+1]); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if want := "a" + Marker + "b" + Marker; out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestRedactorAdjacentAndOverlappingPatterns(t *testing.T) {
	short := []byte("abcdefgh")
	long := []byte("abcdefghij")
	r := NewRedactor([][]byte{short, long})

	// The longer pattern wins, so no "ij" tail is left behind.
	if got := string(r.Redact(long)); got != Marker {
		t.Errorf("longest match: got %q, want %q", got, Marker)
	}
	// Back-to-back occurrences each get their own marker.
	if got := string(r.Redact(append(bytes.Clone(short), short...))); got != Marker+Marker {
		t.Errorf("adjacent: got %q, want %q", got, Marker+Marker)
	}
}

func TestRedactorPassesCleanStreams(t *testing.T) {
	clean := []byte("job finished: 3 changed, 0 failed\n")

	for name, r := range map[string]*Redactor{
		"no patterns":    NewRedactor(nil),
		"empty patterns": NewRedactor([][]byte{{}, nil}),
		"absent secret":  NewRedactor([][]byte{[]byte("never-appears")}),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			w := r.Wrap(&out)
			if _, err := w.Write(clean); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !bytes.Equal(out.Bytes(), clean) {
				t.Errorf("got %q, want %q", out.Bytes(), clean)
			}
		})
	}
}

func TestRedactorRejectsWriteAfterClose(t *testing.T) {
	w := NewRedactor([][]byte{[]byte("x")}).Wrap(io.Discard)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Write([]byte("more")); err == nil {
		t.Fatal("Write after Close was accepted")
	}
}

// TestRedactorScrubsEnvelopeMaterial is the case that matters operationally:
// a derived restic password quoted into a job event line.
func TestRedactorScrubsEnvelopeMaterial(t *testing.T) {
	env, _ := mustInit(t, t.TempDir(), nil)
	pw := env.Derive("restic-repo", "nas")

	r := NewRedactor([][]byte{env.dek, pw})
	line := "restic -r /srv/nas init  # RESTIC_PASSWORD=" +
		base64.StdEncoding.EncodeToString(pw) + " dek=" + hex.EncodeToString(env.dek) + "\n"

	got := r.Redact([]byte(line))
	if bytes.Contains(got, []byte(base64.StdEncoding.EncodeToString(pw))) {
		t.Error("derived password survived redaction")
	}
	if bytes.Contains(got, []byte(hex.EncodeToString(env.dek))) {
		t.Error("hex DEK survived redaction")
	}
	if !bytes.HasPrefix(got, []byte("restic -r /srv/nas init")) {
		t.Errorf("surrounding text mangled: %q", got)
	}
}
