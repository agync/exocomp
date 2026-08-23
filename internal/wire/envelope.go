package wire

import (
	"bytes"
	"encoding/json"
	"io"
)

// SchemaVersion is the pinned contract version. A shape change bumps it, because
// parsers and prompt caches key on it. It starts at 1.
const SchemaVersion = 1

// KindError is the kind of a failure envelope.
const KindError = "error"

// Error is the machine-branchable failure body, shaped after RFC 9457: a stable
// machine code, a human message, and an actionable hint.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// successEnvelope wraps a payload. Data has no omitempty: an empty list must
// still render as "data":[], never vanish, so an agent can tell "no rows" from
// "wrong kind".
type successEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Data          any    `json:"data"`
}

// errorEnvelope is the failure shape. It carries no data field at all.
type errorEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Error         Error  `json:"error"`
}

// WriteSuccess marshals a success envelope for kind and data and writes it,
// terminated by a newline. data should be a projection type from this package;
// a list projection must be a non-nil slice so an empty list renders "[]".
func WriteSuccess(w io.Writer, kind string, data any) error {
	return encode(w, successEnvelope{SchemaVersion: SchemaVersion, Kind: kind, Data: data})
}

// WriteError maps err onto the taxonomy and writes an error envelope.
func WriteError(w io.Writer, err error) error {
	return WriteErrorValue(w, ErrorFor(err))
}

// WriteErrorValue writes a pre-built error envelope. The auth boundary, which
// must not be imported here, uses this to emit unauthenticated/forbidden/busy.
func WriteErrorValue(w io.Writer, e Error) error {
	return encode(w, errorEnvelope{SchemaVersion: SchemaVersion, Kind: KindError, Error: e})
}

// MarshalSuccess returns the exact bytes WriteSuccess would write (newline
// included). It is the seam the golden tests pin.
func MarshalSuccess(kind string, data any) ([]byte, error) {
	var b bytes.Buffer
	if err := WriteSuccess(&b, kind, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func encode(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	// The output carries identifiers and states, not HTML; escaping <,>,& to
	// \u00xx only bloats bytes and hurts diffability.
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
