package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newHooksCmd)
}

func newHooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Show and validate a source's pre/post-run hooks and health gate",
		Long: "A source declares ordered pre_run and post_run steps and a post-run " +
			"health/verification gate in .exocomp/hooks.yml (with an operator override). " +
			"`hooks show` prints the resolved lifecycle a run would compose; `hooks validate` " +
			"refuses a malformed file before it can ever run.",
	}
	cmd.AddCommand(newHooksShowCmd(), newHooksValidateCmd())
	return cmd
}

func newHooksShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <source>",
		Short: "Print the resolved hooks + gate for a source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			return showHooks(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
}

func newHooksValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <path>",
		Short: "Parse a hooks.yml and refuse it if malformed",
		Long: "Reads a hooks.yml FILE (not a source) and reports whether it is well-formed: " +
			"unknown keys, a step naming neither run nor script, or a gate probe that cannot " +
			"decide are all refused. Exits non-zero on a malformed file, so it can gate CI.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return validateHooksFile(cmd.OutOrStdout(), args[0])
		},
	}
}

// showHooks resolves and prints a source's lifecycle: which file is in effect,
// the pre/post steps, and the gate probes. It is a dry lister — it reads the
// resolved hooks and touches nothing.
func showHooks(ctx context.Context, out io.Writer, sourceName string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("hooks: resolve data dir: %w", err)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	src, err := st.SourceByName(ctx, sourceName)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("hooks: no source %q", sourceName)
	}
	if err != nil {
		return err
	}

	loc, err := source.Resolve(dir, src.Kind, src.Name, src.URI)
	if err != nil {
		return fmt.Errorf("hooks: %w", err)
	}

	hk, prov, err := hooks.ResolveWithSource(loc.Root, hooksOverridePath(dir, src.Name))
	if err != nil {
		return err
	}

	switch {
	case prov.Path == "":
		_, _ = fmt.Fprintf(out, "source %s: no hooks.yml (a run is just its play)\n", src.Name)
		return nil
	case prov.IsOverride:
		_, _ = fmt.Fprintf(out, "source %s: hooks from operator override %s\n", src.Name, prov.Path)
	default:
		_, _ = fmt.Fprintf(out, "source %s: hooks from %s\n", src.Name, prov.Path)
	}
	printHookList(out, "pre_run", hk.PreRun)
	printHookList(out, "post_run", hk.PostRun)
	if len(hk.Gate) > 0 {
		_, _ = fmt.Fprintln(out, "gate:")
		for _, p := range hk.Gate {
			_, _ = fmt.Fprintf(out, "  %-16s %s\n", p.Name, probeKind(p))
		}
	}
	if hk.Empty() {
		_, _ = fmt.Fprintln(out, "  (no steps, no gate)")
	}
	return nil
}

func printHookList(out io.Writer, phase string, list []hooks.Hook) {
	if len(list) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "%s:\n", phase)
	for _, h := range list {
		what := "run: " + h.Run
		if h.Script != "" {
			what = "script: " + h.Script
		}
		secret := ""
		if h.Secret != "" {
			secret = "  [secret " + h.Secret + "]"
		}
		_, _ = fmt.Fprintf(out, "  %-16s %s%s\n", h.Name, what, secret)
	}
}

// validateHooksFile parses a hooks.yml file and reports the first problem. A
// malformed file exits non-zero (the error a cobra RunE returns), which is what
// lets it gate a repo's CI before a bad hooks.yml is ever registered.
func validateHooksFile(out io.Writer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("hooks: read %s: %w", path, err)
	}
	hk, err := hooks.Parse(data)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s: ok (%d pre_run, %d post_run, %d gate probe(s))\n",
		path, len(hk.PreRun), len(hk.PostRun), len(hk.Gate))
	return nil
}
