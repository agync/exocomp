package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newCommandsCmd)
}

// commandNode is the self-describing projection of one command in the tree: what
// a harness needs to discover and drive exocomp in a single call. It is
// CLI-specific (it reads cobra/pflag) so it lives here, not in wire.
type commandNode struct {
	Name     string        `json:"name"`
	Path     string        `json:"path"`
	Short    string        `json:"short,omitempty"`
	Example  string        `json:"example,omitempty"`
	Hidden   bool          `json:"hidden,omitempty"`
	Runnable bool          `json:"runnable"`
	Flags    []commandFlag `json:"flags,omitempty"`
	Commands []commandNode `json:"commands,omitempty"`
}

type commandFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type,omitempty"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage,omitempty"`
}

// fillExamples gives every runnable command a copy-pasteable Example. Read and
// list verbs carry an authored one; the rest are filled from their own Use, so
// the tree is fully self-describing and the CI guard (every command has an
// Example) never rots as new commands land. It only fills empties, so an
// authored Example always wins.
func fillExamples(root *cobra.Command) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() && c.Example == "" {
			c.Example = generatedExample(c)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// generatedExample is CommandPath plus the argument placeholders from Use, e.g.
// Use "apply <name>" under `exocomp workspace` yields
// "exocomp workspace apply <name>".
func generatedExample(c *cobra.Command) string {
	if fields := strings.Fields(c.Use); len(fields) > 1 {
		return c.CommandPath() + " " + strings.Join(fields[1:], " ")
	}
	return c.CommandPath()
}

func newCommandsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "commands",
		Short:   "Dump the command tree so a harness self-describes in one call",
		Example: "exocomp commands --output json",
		Hidden:  true,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			// Describe from the root so the whole tree — not just this
			// subcommand — is dumped.
			root := cmd.Root()
			node := describeCommand(root)
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindCommands, node)
			}
			printCommandTree(cmd, node, 0)
			return nil
		},
	}
	return cmd
}

func describeCommand(c *cobra.Command) commandNode {
	node := commandNode{
		Name:     c.Name(),
		Path:     c.CommandPath(),
		Short:    c.Short,
		Example:  c.Example,
		Hidden:   c.Hidden,
		Runnable: c.Runnable(),
	}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		node.Flags = append(node.Flags, commandFlag{
			Name:      f.Name,
			Shorthand: f.Shorthand,
			Type:      f.Value.Type(),
			Default:   f.DefValue,
			Usage:     f.Usage,
		})
	})
	for _, sub := range c.Commands() {
		node.Commands = append(node.Commands, describeCommand(sub))
	}
	return node
}

func printCommandTree(cmd *cobra.Command, node commandNode, depth int) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "%s%s\t%s\n", strings.Repeat("  ", depth), node.Path, node.Short)
	for _, sub := range node.Commands {
		printCommandTree(cmd, sub, depth+1)
	}
}
