package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/wire"
)

// This file is the client-mode seam. When a remote is selected (--remote flag or
// EXOCOMP_REMOTE env; a bare command with neither stays LOCAL), a read verb's
// RunE short-circuits into remoteClient and renders the daemon's wire projection
// instead of touching a local data dir. --output json re-emits the decoded view
// through wire.WriteSuccess, so remote bytes equal local bytes; text is rendered
// here from the same views. The WRITE half of the seam — the verbs that enqueue
// on the remote daemon and follow the job — lives in remotewrite.go.

// remoteClient resolves the selected remote into a configured, pinned client.
// The bool is false in LOCAL mode (no --remote, no EXOCOMP_REMOTE) — the caller
// then runs its normal local path unchanged. An error means a remote WAS asked
// for but could not be resolved (no default, not pinned, no token): the command
// fails rather than silently falling back to local, which would run an operator's
// intended-remote read against the wrong box.
func remoteClient(cmd *cobra.Command) (*client.Client, bool, error) {
	selector, active := selectedRemote(cmd)
	if !active {
		return nil, false, nil
	}
	cfg, err := loadHostsConfig()
	if err != nil {
		return nil, false, err
	}

	// An empty selector (`--remote=`) means the hosts.toml default.
	if selector == "" {
		if cfg.Default == "" {
			return nil, false, errors.New("no default remote configured; pass --remote <endpoint> or run `exocomp login`")
		}
		selector = cfg.Default
	}

	entry, ok := cfg.Hosts[selector]
	if !ok {
		// Not a stored alias: maybe a raw endpoint that WAS pinned under some
		// alias. Match by endpoint so `--remote https://box:8443` works after a
		// login. An unpinned endpoint is refused — TOFU demands a login first.
		if e, found := entryByEndpoint(cfg, selector); found {
			entry = e
		} else {
			return nil, false, fmt.Errorf(
				"remote %q is not pinned; run `exocomp login %s` first", selector, selector)
		}
	}

	token := resolveToken(cmd, entry)
	c, err := client.New(client.Config{Endpoint: entry.Endpoint, Fingerprint: entry.Fingerprint, Token: token})
	if err != nil {
		return nil, false, err
	}
	return c, true, nil
}

// selectedRemote returns the remote selector and whether remote mode is active.
// Precedence: --remote flag, then EXOCOMP_REMOTE. The hosts.toml default is NOT
// consulted here — it only fills a valueless --remote, downstream — so a bare
// command with neither flag nor env stays local even when a default is stored.
func selectedRemote(cmd *cobra.Command) (string, bool) {
	if f := cmd.Flags().Lookup(remoteFlag); f != nil && f.Changed {
		return f.Value.String(), true
	}
	if env := strings.TrimSpace(os.Getenv(remoteEnv)); env != "" {
		return env, true
	}
	return "", false
}

// entryByEndpoint finds a pinned entry whose stored endpoint equals selector, so
// a raw endpoint on --remote resolves to the pin login recorded for it.
func entryByEndpoint(cfg hostsConfig, endpoint string) (hostEntry, bool) {
	for _, e := range cfg.Hosts {
		if e.Endpoint == endpoint {
			return e, true
		}
	}
	return hostEntry{}, false
}

// remoteFail renders a remote error: on the JSON path it emits a wire error
// envelope, preserving an APIError's taxonomy code (which wire.ErrorFor cannot
// recover, since an APIError is not a store sentinel) so an agent still branches
// on code; on the text path it returns the error for cobra to print. Either way
// the error is returned so the process exits nonzero, matching the local paths.
func remoteFail(cmd *cobra.Command, mode string, err error) error {
	if mode != outputJSON {
		return err
	}
	cmd.SilenceErrors = true
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		_ = wire.WriteErrorValue(cmd.OutOrStdout(),
			wire.Error{Code: apiErr.Code, Message: apiErr.Message, Hint: apiErr.Hint})
	} else {
		_ = wire.WriteError(cmd.OutOrStdout(), err)
	}
	return err
}

// fmtWireTS reformats an RFC3339 wire timestamp into the "2006-01-02 15:04:05Z"
// the local text renderers print, so remote text matches local text. An
// unparseable or empty value falls back to a dash / the raw string rather than
// erroring — text is a display aid, not a decision input.
func fmtWireTS(s string) string {
	if s == "" {
		return "-"
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02 15:04:05Z")
	}
	return s
}

func exitCodeLabelPtr(code *int) string {
	if code == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *code)
}

// --- remote renderers: one per wired verb, mirroring the local text columns ---

func renderRemoteJobsList(cmd *cobra.Command, mode string, rows []wire.JobView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindJobList, rows)
	}
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "no jobs")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATE\tRUNBOOK\tSOURCE\tKIND\tTRIGGER\tEXIT\tQUEUED")
	for _, j := range rows {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			j.ID, j.State, dashIfEmpty(j.Runbook), dashIfEmpty(j.Source), dashIfEmpty(j.Kind),
			dashIfEmpty(j.Trigger), exitCodeLabelPtr(j.ExitCode), fmtWireTS(j.QueuedAt))
	}
	return w.Flush()
}

func renderRemoteJobGet(cmd *cobra.Command, mode string, j wire.JobView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindJob, j)
	}
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "id\t%d\n", j.ID)
	_, _ = fmt.Fprintf(w, "state\t%s\n", j.State)
	_, _ = fmt.Fprintf(w, "runbook\t%s\n", dashIfEmpty(j.Runbook))
	_, _ = fmt.Fprintf(w, "source\t%s\n", dashIfEmpty(j.Source))
	_, _ = fmt.Fprintf(w, "kind\t%s\n", dashIfEmpty(j.Kind))
	_, _ = fmt.Fprintf(w, "trigger\t%s\n", dashIfEmpty(j.Trigger))
	_, _ = fmt.Fprintf(w, "exit\t%s\n", exitCodeLabelPtr(j.ExitCode))
	_, _ = fmt.Fprintf(w, "queued\t%s\n", fmtWireTS(j.QueuedAt))
	return w.Flush()
}

func renderRemoteJobEvents(cmd *cobra.Command, mode string, rows []wire.JobEventView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindJobEvents, rows)
	}
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "no events")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SEQ\tTS\tLEVEL\tMSG")
	for _, e := range rows {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", e.Seq, fmtWireTS(e.Ts), e.Level, e.Msg)
	}
	return w.Flush()
}

func renderRemoteWorkspaces(cmd *cobra.Command, mode string, rows []wire.WorkspaceView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindWorkspaceList, rows)
	}
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "no workspaces")
		return nil
	}
	for _, ws := range rows {
		_, _ = fmt.Fprintf(out, "%-20s serial %-6d %s\n", ws.Name, ws.Serial, ws.TofuDir)
	}
	return nil
}

func renderRemoteSources(cmd *cobra.Command, mode string, rows []wire.SourceView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindSourceList, rows)
	}
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "no sources")
		return nil
	}
	for _, s := range rows {
		pin := s.Ref
		if s.LastSyncSHA != "" {
			pin = fmt.Sprintf("%s@%s", s.Ref, short(s.LastSyncSHA))
		}
		flags := ""
		if s.Signed {
			flags += " signed"
		}
		if !s.Enabled {
			flags += " disabled"
		}
		_, _ = fmt.Fprintf(out, "%-20s %-9s %-8s %s%s\n", s.Name, s.Kind, pin, s.URI, flags)
	}
	return nil
}

func renderRemoteApprovals(cmd *cobra.Command, mode string, rows []wire.ApprovalView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindApprovalList, rows)
	}
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "nothing is waiting on you")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	// The remote list omits the local RUNBOOK column: it is a second per-row
	// GetJob the read API does not fold in, and rendering here does not add a store
	// round-trip the daemon did not already make.
	_, _ = fmt.Fprintln(w, "ID\tSTATE\tJOB\tWORKSPACE\tPLAN SERIAL\tEXPIRES\tVIA")
	for _, a := range rows {
		serial := "-"
		if a.PlanSerial > 0 {
			serial = fmt.Sprintf("%d", a.PlanSerial)
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\t%s\n",
			a.ID, a.State, a.JobID, dashIfEmpty(a.Workspace), serial,
			fmtWireTS(a.ExpiresAt), dashIfEmpty(a.DecidedVia))
	}
	return w.Flush()
}

func renderRemoteHealth(cmd *cobra.Command, mode string, view wire.HealthView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindDoctor, view)
	}
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, e := range view {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", strings.ToUpper(e.Status), e.Name, e.Detail)
	}
	return w.Flush()
}
