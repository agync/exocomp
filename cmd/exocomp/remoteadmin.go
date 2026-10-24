package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// This file holds the remote paths of the source, secrets and run-secret
// verbs (Decision 0027). Each sends the verb to the selected daemon and prints
// what the local verb prints; under --output json it re-emits the daemon's
// envelope. Inputs that live on this machine (a secret read from stdin, an
// allowed-signers file) are read here and travel in the request.

// remoteAdminOut prints a remote admin answer: the daemon's envelope under
// --output json, else the text the local verb prints.
func remoteAdminOut(cmd *cobra.Command, kind string, data any, text func()) error {
	mode, err := outputMode(cmd)
	if err != nil {
		return err
	}
	if mode == outputJSON {
		return emitJSON(cmd, kind, data)
	}
	text()
	return nil
}

// remoteAdminFail renders a remote refusal with the verb's prefix on the text
// path, as the local verb's errors carry one.
func remoteAdminFail(cmd *cobra.Command, verb string, err error) error {
	mode, merr := outputMode(cmd)
	if merr != nil {
		return merr
	}
	if mode != outputJSON {
		return fmt.Errorf("%s: %w", verb, err)
	}
	return remoteFail(cmd, mode, err)
}

// --- sources ---------------------------------------------------------------

func runRemoteSourceAdd(cmd *cobra.Command, rc *client.Client, name string, spec sourceAddSpec) error {
	req := client.SourceAddRequest{
		Name: name, Path: spec.path, URL: spec.url, Writable: spec.writable,
		Signed: spec.signed, AllowedSigners: spec.allowedSigners,
	}
	if spec.url != "" {
		// --ref defaults to main for every kind; only a git source pins one.
		req.Ref = spec.ref
	}
	res, err := rc.AddSource(cmd.Context(), req)
	if err != nil {
		return remoteAdminFail(cmd, "source add", err)
	}
	return remoteAdminOut(cmd, client.KindSourceAdded, res, func() {
		out := cmd.OutOrStdout()
		src := res.Source
		if src.Kind == store.SourcePath {
			access := "read-only"
			if src.Writable {
				access = "writable"
			}
			_, _ = fmt.Fprintf(out, "registered %s path source %q at %s\n", access, src.Name, src.URI)
			return
		}
		_, _ = fmt.Fprintf(out, "registered git source %q (%s, ref %s)\n", src.Name, src.URI, src.Ref)
		_, _ = fmt.Fprintln(out, "add this deploy key to the forge as a read-only key:")
		_, _ = fmt.Fprintln(out, res.PublicKey)
	})
}

func runRemoteSourceSync(cmd *cobra.Command, rc *client.Client, name, ref string) error {
	res, err := rc.SyncSource(cmd.Context(), name, ref)
	if err != nil {
		return remoteAdminFail(cmd, "source sync", err)
	}
	return remoteAdminOut(cmd, client.KindSourceSynced, res, func() {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "synced %q to %s (%s)\n", res.Name, res.Ref, short(res.SHA))
	})
}

func runRemoteSourceRm(cmd *cobra.Command, rc *client.Client, name string) error {
	res, err := rc.RemoveSource(cmd.Context(), name)
	if err != nil {
		return remoteAdminFail(cmd, "source rm", err)
	}
	return remoteAdminOut(cmd, client.KindSourceRemoved, res, func() {
		if res.CachesRemoved {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed source %q (cached project deps removed)\n", name)
			return
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed source %q\n", name)
	})
}

func runRemoteSourceEgress(cmd *cobra.Command, rc *client.Client, name string, ports []int) error {
	src, err := rc.SetSourceEgress(cmd.Context(), name, ports)
	if err != nil {
		return remoteAdminFail(cmd, "source egress", err)
	}
	return remoteAdminOut(cmd, client.KindSource, src, func() { printSourceEgress(cmd, src.Name, src.ExtraEgressPorts) })
}

func runRemoteSourceRunSecrets(cmd *cobra.Command, rc *client.Client, name, binding string) error {
	src, err := rc.SetSourceRunSecrets(cmd.Context(), name, binding)
	if err != nil {
		return remoteAdminFail(cmd, "source run-secrets", err)
	}
	return remoteAdminOut(cmd, client.KindSource, src, func() { printSourceRunSecrets(cmd, src.Name, src.DefaultRunSecrets) })
}

// --- secrets ---------------------------------------------------------------

func runRemoteSecretAdd(cmd *cobra.Command, rc *client.Client, req client.SecretAddRequest) error {
	res, err := rc.SealSecret(cmd.Context(), req)
	if err != nil {
		return remoteAdminFail(cmd, "secrets add", err)
	}
	return remoteAdminOut(cmd, client.KindSecretSealed, res, func() {
		printSecretSealed(cmd, res.Name, res.Kind, res.Source, res.Bytes, parseWireTime(&res.ExpiresAt))
	})
}

// runRemoteSecretReplace reads the secret's source from the daemon first: a
// generated secret is minted there, so only a user secret's replacement reads
// a new value from stdin, as the local verb does.
func runRemoteSecretReplace(cmd *cobra.Command, rc *client.Client, name string, generate, setExpiry bool,
	expiresAt *time.Time) error {
	views, err := rc.ListSecrets(cmd.Context())
	if err != nil {
		return remoteAdminFail(cmd, "secrets replace", err)
	}
	var existing *wire.SecretView
	for i := range views {
		if views[i].Name == name {
			existing = &views[i]
		}
	}
	if existing == nil {
		return remoteAdminFail(cmd, "secrets replace",
			&client.APIError{Status: http.StatusNotFound, Code: wire.CodeNotFound, Message: "no secret " + name})
	}
	// Refused before a value is read, as the daemon would refuse it after.
	if service.IsSystemSecret(existing.Name, existing.Kind) {
		return remoteAdminFail(cmd, "secrets replace", &client.APIError{Status: http.StatusForbidden,
			Code: wire.CodeForbidden, Message: "secret " + name + " is one exocomp makes for itself; " +
				"replace it on the daemon's host with --local"})
	}
	req := client.SecretReplaceRequest{Generate: generate, SetExpiry: setExpiry, ExpiresAt: formatExpiry(expiresAt)}
	if !generate && existing.Source != store.SecretSourceGenerated {
		value, err := readSecretValue(cmd.InOrStdin())
		if err != nil {
			return fmt.Errorf("secrets replace: read value from stdin (a user secret needs a new value): %w", err)
		}
		defer zero(value)
		req.Value = value
	}
	res, err := rc.ReplaceSecret(cmd.Context(), name, req)
	if err != nil {
		return remoteAdminFail(cmd, "secrets replace", err)
	}
	return remoteAdminOut(cmd, client.KindSecretReplaced, res, func() {
		printSecretReplaced(cmd, res.Name, res.Rotated, res.Regenerated, res.Bytes, res.SetExpiry,
			parseWireTime(&res.ExpiresAt))
	})
}

// runRemoteSecretRm deletes a secret, asking for the same confirmation the local
// verb does when the daemon reports bindings that still map it.
func runRemoteSecretRm(cmd *cobra.Command, rc *client.Client, name string, force bool) error {
	res, err := rc.RemoveSecret(cmd.Context(), name, false)
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Code == wire.CodeConflict {
		if cerr := confirmDestructive(cmd, force, fmt.Sprintf("secrets rm %q (still bound)", name), "--force"); cerr != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "refusing to delete: %s\n", apiErr.Message)
			mode, merr := outputMode(cmd)
			if merr != nil {
				return merr
			}
			return remoteFail(cmd, mode, cerr)
		}
		res, err = rc.RemoveSecret(cmd.Context(), name, true)
	}
	if err != nil {
		return remoteAdminFail(cmd, "secrets rm", err)
	}
	return remoteAdminOut(cmd, client.KindSecretRemoved, res, func() {
		forced := ""
		if res.ForcedBindings > 0 {
			forced = fmt.Sprintf(" (forced past %d binding reference(s))", res.ForcedBindings)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deleted %q%s\n", res.Name, forced)
	})
}

// --- run-secret bindings ---------------------------------------------------

func runRemoteRunSecretAdd(cmd *cobra.Command, rc *client.Client, binding, envName, secret string) error {
	pair, err := rc.BindRunSecret(cmd.Context(), binding, envName, secret)
	if err != nil {
		return remoteAdminFail(cmd, "run-secret add", err)
	}
	return remoteAdminOut(cmd, client.KindRunSecretBound, pair, func() {
		printRunSecretBound(cmd, pair.EnvName, pair.Secret, pair.Binding)
	})
}

func runRemoteRunSecretList(cmd *cobra.Command, rc *client.Client, binding string) error {
	if binding == "" {
		names, err := rc.RunSecretBindings(cmd.Context())
		if err != nil {
			return remoteAdminFail(cmd, "run-secret list", err)
		}
		return remoteAdminOut(cmd, client.KindRunSecretList, names, func() { printRunSecretBindings(cmd, names) })
	}
	pairs, err := rc.RunSecretPairs(cmd.Context(), binding)
	if err != nil {
		return remoteAdminFail(cmd, "run-secret list", err)
	}
	return remoteAdminOut(cmd, client.KindRunSecretPairs, pairs, func() {
		rows := make([]store.RunSecret, 0, len(pairs))
		for _, p := range pairs {
			rows = append(rows, store.RunSecret{Binding: p.Binding, EnvName: p.EnvName, SecretName: p.Secret})
		}
		printRunSecretPairs(cmd, binding, rows)
	})
}

func runRemoteRunSecretRm(cmd *cobra.Command, rc *client.Client, binding string) error {
	if err := rc.RemoveRunSecretBinding(cmd.Context(), binding); err != nil {
		return remoteAdminFail(cmd, "run-secret rm", err)
	}
	return remoteAdminOut(cmd, client.KindRunSecretRemove, map[string]string{"binding": binding}, func() {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed binding %q\n", binding)
	})
}
