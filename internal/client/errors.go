package client

import (
	"fmt"

	"exocomp.dev/exocomp/internal/wire"
)

// APIError is a remote failure surfaced from the daemon's wire error envelope.
// It carries the taxonomy Code an agent branches on (wire.CodeNotFound and the
// rest), the human Message, the actionable Hint, and the HTTP Status the daemon
// answered with. It maps back onto the CLI's exit-code table via ExitCode, so a
// remote not_found exits 3 exactly as a local one does.
type APIError struct {
	Status  int
	Code    string
	Message string
	Hint    string
}

func (e *APIError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ExitCode maps the remote error onto the shared process exit-code table
// (wire.ExitForCode), so the CLI exits with the same code whether the failure
// was decided locally or by the daemon.
func (e *APIError) ExitCode() int { return wire.ExitForCode(e.Code) }

// PinMismatchError is returned when the certificate the daemon presents does not
// match the pinned fingerprint. It is deliberately loud and terminal: a changed
// cert on a bare tailnet is a re-key the operator must confirm out of band, or a
// machine-in-the-middle at connect time. The connection is refused; nothing is
// sent over it.
type PinMismatchError struct {
	Endpoint string
	Expected string
	Got      string
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf(
		"client: certificate fingerprint for %s changed\n  pinned:    %s\n  presented: %s\n"+
			"refusing to connect; if this is an expected re-key, `exocomp login %s` again to re-pin",
		e.Endpoint, e.Expected, e.Got, e.Endpoint)
}
