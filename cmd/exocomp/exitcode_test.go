package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestExitCodeForTable drives representative local (sentinel-wrapped) and
// remote (client.APIError) errors through exitCodeFor and asserts each lands
// on the code docs/operations/machine-output.md documents. A generic error —
// one that carries no taxonomy sentinel and no APIError — must still exit 1,
// exactly as it did before this mapping existed.
func TestExitCodeForTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"local not found", fmt.Errorf("store: get job 999999: %w", store.ErrNotFound), wire.ExitNotFound},
		{"local conflict", fmt.Errorf("store: transition: %w", store.ErrStateConflict), wire.ExitConflict},
		{"local concurrency held", fmt.Errorf("queue: claim: %w", store.ErrConcurrencyHeld), wire.ExitConflict},
		{"local stale plan", fmt.Errorf("reactive: apply: %w", reactive.ErrStalePlan), wire.ExitConflict},
		{"local duplicate has no code above conflict", fmt.Errorf("store: create: %w", store.ErrDuplicate), wire.ExitFailure},
		{"local generic error", errors.New("command: something broke"), wire.ExitFailure},

		{"remote not found", &client.APIError{Status: http.StatusNotFound, Code: wire.CodeNotFound, Message: "no such job"}, wire.ExitNotFound},
		{"remote unauthenticated", &client.APIError{Status: http.StatusUnauthorized, Code: wire.CodeUnauthenticated, Message: "no bearer"}, wire.ExitAuth},
		{"remote forbidden", &client.APIError{Status: http.StatusForbidden, Code: wire.CodeForbidden, Message: "scope too low"}, wire.ExitAuth},
		{"remote conflict", &client.APIError{Status: http.StatusConflict, Code: wire.CodeConflict, Message: "not in expected state"}, wire.ExitConflict},
		{"remote stale plan", &client.APIError{Status: http.StatusConflict, Code: wire.CodeStalePlan, Message: "workspace moved"}, wire.ExitConflict},
		{"remote kdf busy", &client.APIError{Status: http.StatusServiceUnavailable, Code: wire.CodeKDFBusy, Message: "auth gate saturated"}, wire.ExitBusy},
		{"remote internal", &client.APIError{Status: http.StatusInternalServerError, Code: wire.CodeInternal, Message: "boom"}, wire.ExitFailure},
		{"remote bare 404 (no envelope)", statusOnlyAPIError(http.StatusNotFound), wire.ExitNotFound},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCodeFor(c.err); got != c.want {
				t.Errorf("exitCodeFor(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// statusOnlyAPIError mirrors what client.statusError builds for a non-envelope
// HTTP failure (a bare 404 with no wire body), so the table covers that path
// without importing the unexported constructor.
func statusOnlyAPIError(status int) *client.APIError {
	return &client.APIError{Status: status, Code: wire.CodeNotFound, Message: fmt.Sprintf("remote returned HTTP %d", status)}
}

// TestExitCodeLocalNotFoundInProcess proves the wiring end to end for the
// error path a real `jobs get <missing id>` returns from RunE: the returned
// error is store.ErrNotFound (wrapped), and exitCodeFor maps it to 3 exactly
// as the process-level test below observes for the real binary.
func TestExitCodeLocalNotFoundInProcess(t *testing.T) {
	seedStore(t)

	_, err := run(t, "jobs", "get", "999999", "--output", "json")
	if err == nil {
		t.Fatal("jobs get on a missing id must fail")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v, want it to wrap store.ErrNotFound", err)
	}
	if got := exitCodeFor(err); got != wire.ExitNotFound {
		t.Fatalf("exitCodeFor(%v) = %d, want %d", err, got, wire.ExitNotFound)
	}
}

// TestExitCodeRemoteMappingInProcess drives the client-mode seam (runRoot,
// pinnedRemote — the same helpers remotewrite_test.go and remote_test.go use)
// against stub daemons answering with each taxonomy code, and asserts the
// error Execute() returns maps to the documented exit code.
func TestExitCodeRemoteMappingInProcess(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		want   int
	}{
		{"not found", http.StatusNotFound, wire.CodeNotFound, wire.ExitNotFound},
		{"forbidden", http.StatusForbidden, wire.CodeForbidden, wire.ExitAuth},
		{"unauthenticated", http.StatusUnauthorized, wire.CodeUnauthenticated, wire.ExitAuth},
		{"conflict", http.StatusConflict, wire.CodeConflict, wire.ExitConflict},
		{"kdf busy", http.StatusServiceUnavailable, wire.CodeKDFBusy, wire.ExitBusy},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pinnedRemote(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_ = wire.WriteErrorValue(w, wire.NewError(c.code, "stub"))
			}))

			_, _, err := runRoot(t, []string{"--remote", "box", "jobs", "get", "1"})
			if err == nil {
				t.Fatalf("%s: remote call must fail", c.name)
			}
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("%s: error = %v, want a *client.APIError", c.name, err)
			}
			if got := exitCodeFor(err); got != c.want {
				t.Errorf("%s: exitCodeFor = %d, want %d", c.name, got, c.want)
			}
		})
	}
}

// TestExitCodeRealCLILocalNotFound runs the actual built binary end to end —
// through main(), not Execute() alone — so the os.Exit(exitCodeFor(err)) site
// is the thing under test, not a stand-in for it. `jobs get 999999` against a
// freshly-initialized, empty data dir is the closest reproducible not_found.
func TestExitCodeRealCLILocalNotFound(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	runCLI(t, bin, "init")

	out, code := runCLIExit(t, bin, "jobs", "get", "999999", "--output", "json")
	if code != wire.ExitNotFound {
		t.Fatalf("exocomp jobs get 999999 exited %d, want %d:\n%s", code, wire.ExitNotFound, out)
	}
	if !strings.Contains(out, `"code":"not_found"`) {
		t.Fatalf("output missing not_found envelope: %s", out)
	}
}

// TestExitCodeRealCLIRemoteForbidden runs the actual built binary against a
// real pinned TLS remote that answers 403/forbidden, proving the remote leg of
// the same wiring: client.APIError.ExitCode() reaching os.Exit through main().
func TestExitCodeRealCLIRemoteForbidden(t *testing.T) {
	// pinnedRemote writes hosts.toml under XDG_CONFIG_HOME (via t.Setenv), which
	// exec.Command's default nil Env inherits from os.Environ() — so the
	// subprocess below sees the exact same pin the in-process client would.
	pinnedRemote(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = wire.WriteErrorValue(w, wire.NewError(wire.CodeForbidden, "this token's scope is too low"))
	}))

	bin := buildExocomp(t)
	out, code := runCLIExit(t, bin, "--remote", "box", "jobs", "get", "1", "--output", "json")
	if code != wire.ExitAuth {
		t.Fatalf("exocomp --remote box jobs get 1 exited %d, want %d:\n%s", code, wire.ExitAuth, out)
	}
	if !strings.Contains(out, `"code":"forbidden"`) {
		t.Fatalf("output missing forbidden envelope: %s", out)
	}
}
