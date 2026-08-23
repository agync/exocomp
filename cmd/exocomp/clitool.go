package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newCLIToolCmd)
}

func newCLIToolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cli-tool",
		Short: "Register the binaries a governed `run-cli` job may execute",
		Long: "The allow-list behind `exocomp run-cli`. A CLI run — a kubectl, a scoped " +
			"token refresh, a vendor deploy tool — executes only a binary registered here, " +
			"and a name that is not registered is refused before a job row exists. That is " +
			"what makes the kind governable: the queue, the sandbox, the run-secret plane " +
			"and the approval gate all apply, and the set of programs they can apply to is " +
			"an operator decision recorded in the database.\n\n" +
			"Entries live in the database beside sources, inventories and runners, so the " +
			"boundary deciding what may execute is restored by the same backup the rest of " +
			"the control plane is (docs/operations/cli-run.md).",
	}
	cmd.AddCommand(newCLIToolAddCmd(), newCLIToolListCmd(), newCLIToolRmCmd())
	return cmd
}

func newCLIToolAddCmd() *cobra.Command {
	var (
		binary      string
		fixedArgs   []string
		argPattern  string
		egress      string
		description string
	)
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a binary to the CLI allow-list",
		Long: "Registers one executable as runnable by `run-cli`. --binary is exec'd " +
			"directly: it is one token, never a command line a shell would split, and a " +
			"value carrying whitespace is refused.\n\n" +
			"--fixed-arg is repeatable and is prepended to every invocation, which is how " +
			"the part that must not vary (`--context prod`, `--namespace apps`) stops " +
			"depending on each caller repeating it. --arg-pattern anchors an operator's own " +
			"arguments to a regexp, narrowing a broad tool to the shape it is registered " +
			"for. --egress is the run's whole network policy: `none` (the default) reaches " +
			"nothing, `https` opens port 443 and nothing else.\n\n" +
			"Registering widens what this control plane can execute, so it takes the " +
			"destructive-confirm contract's --assume-yes.",
		Example: "exocomp cli-tool add kubectl --binary kubectl --fixed-arg --context=homelab --egress https --assume-yes",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tool := store.CLITool{
				Name:        args[0],
				Binary:      binary,
				FixedArgs:   fixedArgs,
				ArgPattern:  argPattern,
				Egress:      egress,
				Description: description,
			}
			if tool.Binary == "" {
				tool.Binary = tool.Name
			}
			if err := store.ValidateCLITool(tool); err != nil {
				return fmt.Errorf("cli-tool add: %w", err)
			}
			if err := confirmDestructive(cmd, false,
				fmt.Sprintf("allow-listing %q for governed CLI runs", tool.Binary), ""); err != nil {
				return err
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			created, err := st.CreateCLITool(cmd.Context(), tool)
			if err != nil {
				return fmt.Errorf("cli-tool add: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"allow-listed %q -> %s (egress %s)\n", created.Name, created.Binary, created.Egress)
			return nil
		},
	}
	add.Flags().StringVar(&binary, "binary", "",
		"the executable: a name resolved on PATH or an absolute path (default: the tool name)")
	add.Flags().StringArrayVar(&fixedArgs, "fixed-arg", nil,
		"repeatable: an argument prepended to every invocation of this tool")
	add.Flags().StringVar(&argPattern, "arg-pattern", "",
		"anchored regexp every operator-supplied argument must match (default: any argument free of control characters)")
	add.Flags().StringVar(&egress, "egress", store.CLIEgressNone,
		"outbound network the run may reach: none|https")
	add.Flags().StringVar(&description, "description", "", "what this tool is registered for")
	return add
}

func newCLIToolListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List the CLI allow-list",
		Example: "exocomp cli-tool list --output json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListCLITools(cmd.Context())
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindCLIToolList, wire.NewCLIToolList(rows))
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no allow-listed cli tools")
				return nil
			}
			for _, t := range rows {
				_, _ = fmt.Fprintf(out, "%-20s %-32s %-6s %s\n",
					t.Name, t.Binary, t.Egress, strings.Join(t.FixedArgs, " "))
			}
			return nil
		},
	}
}

func newCLIToolRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a binary from the CLI allow-list",
		// No confirmation gate: removing an entry only narrows what may execute,
		// and re-adding it is the confirmed operation. The gate belongs on the
		// direction that widens the boundary.
		Example: "exocomp cli-tool rm kubectl",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err := st.DeleteCLITool(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("cli-tool rm: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed cli tool %q\n", args[0])
			return nil
		},
	}
}
