package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strings"
)

// Marker replaces every known secret value in a redacted stream.
const Marker = "[REDACTED]"

// Redactor scrubs known secret values out of byte streams. Each known value is
// matched raw, base64.StdEncoding-encoded, and hex-encoded in both cases,
// because subprocess output carries secrets in all three forms.
type Redactor struct {
	patterns [][]byte
	max      int
}

// NewRedactor compiles the patterns for the given secret values.
func NewRedactor(known [][]byte) *Redactor {
	r := &Redactor{}
	seen := make(map[string]struct{})
	add := func(p []byte) {
		if len(p) == 0 {
			return
		}
		if _, dup := seen[string(p)]; dup {
			return
		}
		seen[string(p)] = struct{}{}
		r.patterns = append(r.patterns, p)
		if len(p) > r.max {
			r.max = len(p)
		}
	}
	for _, k := range known {
		if len(k) == 0 {
			continue
		}
		add(bytes.Clone(k))
		add([]byte(base64.StdEncoding.EncodeToString(k)))
		lower := hex.EncodeToString(k)
		add([]byte(lower))
		add([]byte(strings.ToUpper(lower)))
	}
	return r
}

// Wrap returns a writer forwarding to w with every known pattern replaced.
// Callers must Close it: bytes that could still begin a pattern are held back
// until Close so a secret split across two Writes is still caught.
func (r *Redactor) Wrap(w io.Writer) io.WriteCloser {
	return &redactingWriter{r: r, w: w}
}

// Redact scrubs a complete in-memory buffer.
func (r *Redactor) Redact(b []byte) []byte {
	out, _ := r.scan(b, true)
	return out
}

type redactingWriter struct {
	r    *Redactor
	w    io.Writer
	buf  []byte
	done bool
}

func (rw *redactingWriter) Write(p []byte) (int, error) {
	if rw.done {
		return 0, io.ErrClosedPipe
	}
	rw.buf = append(rw.buf, p...)
	if err := rw.emit(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (rw *redactingWriter) Close() error {
	if rw.done {
		return nil
	}
	rw.done = true
	return rw.emit(true)
}

func (rw *redactingWriter) emit(final bool) error {
	out, carry := rw.r.scan(rw.buf, final)
	rw.buf = carry
	if len(out) == 0 {
		return nil
	}
	_, err := rw.w.Write(out)
	return err
}

// scan replaces every complete pattern occurrence in buf. Unless final, it
// stops short of the trailing max-1 bytes, which are returned as carry: any
// pattern starting there could still be completed by a later Write.
func (r *Redactor) scan(buf []byte, final bool) (out, carry []byte) {
	hold := 0
	if !final && r.max > 0 {
		hold = r.max - 1
	}
	safe := max(len(buf)-hold, 0)

	out = make([]byte, 0, len(buf))
	i := 0
	for i < len(buf) {
		if n := r.matchAt(buf[i:]); n > 0 {
			out = append(out, Marker...)
			i += n
			continue
		}
		if i >= safe {
			break
		}
		out = append(out, buf[i])
		i++
	}
	return out, bytes.Clone(buf[i:])
}

// matchAt returns the length of the longest pattern that is a prefix of b, so
// one pattern being a prefix of another cannot leave a tail behind.
func (r *Redactor) matchAt(b []byte) int {
	best := 0
	for _, p := range r.patterns {
		if len(p) > best && bytes.HasPrefix(b, p) {
			best = len(p)
		}
	}
	return best
}
