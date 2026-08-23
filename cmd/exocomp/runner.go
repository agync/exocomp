package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newRunnerCmd)
}

// deferredNote is the honest status every runner verb repeats: the registry
// stores definitions, and the tier that would turn one into a running container
// is not built.
const deferredNote = "registered for the deferred container tier; not yet executed"

func newRunnerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runner",
		Short: "Register execution-environment runner definitions (" + deferredNote + ")",
		Long: "A runner is a recipe for an execution-environment image: a base, a bootstrap " +
			"script or a Containerfile, system packages, an optional ansible-core pin, and " +
			"the resource caps a run would ask for (byo-runner design §2). Definitions live " +
			"in the database, so they are backed up and audited with the data volume rather " +
			"than in a config tree the volume does not carry.\n\n" +
			"A registered runner is inert data: it is " + deferredNote + ". Nothing in this " +
			"binary builds an image, calls podman, or hands a runner to a job.",
	}
	cmd.AddCommand(newRunnerAddCmd(), newRunnerListCmd(), newRunnerShowCmd(),
		newRunnerRmCmd(), newRunnerImportCmd())
	return cmd
}

// runnerNameMax bounds a name so it stays usable as an image-tag component for
// the deferred build tier.
const runnerNameMax = 64

// guardRunnerName enforces the entity-name shape: a leading alphanumeric then
// lowercase alphanumerics and separators. It refuses at registration rather
// than leaving a name that only fails once something tries to tag an image with
// it.
func guardRunnerName(name string) error {
	if name == "" || len(name) > runnerNameMax {
		return fmt.Errorf("runner name %q must be 1-%d characters", name, runnerNameMax)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_' || r == '.') && i > 0:
		default:
			return fmt.Errorf("runner name %q must start with a lowercase letter or digit "+
				"and hold only [a-z0-9._-]", name)
		}
	}
	return nil
}

// validateRunner enforces the rules the schema states and the operator would
// otherwise discover at build time. It is shared by `add` and `import` so a
// YAML spec is held to exactly the flags' standard.
func validateRunner(r store.Runner) error {
	if err := guardRunnerName(r.Name); err != nil {
		return err
	}
	if r.Mode != store.RunnerModeCached && r.Mode != store.RunnerModeOnTheFly {
		return fmt.Errorf("mode must be %s or %s, not %q",
			store.RunnerModeCached, store.RunnerModeOnTheFly, r.Mode)
	}
	if strings.TrimSpace(r.Base) == "" {
		return fmt.Errorf("runner %q needs a base image", r.Name)
	}
	if r.Bootstrap != "" && r.Containerfile != "" {
		return fmt.Errorf("runner %q gives both a bootstrap and a containerfile; "+
			"they are alternative build models, not a merge", r.Name)
	}
	return nil
}

func newRunnerAddCmd() *cobra.Command {
	var (
		mode          string
		base          string
		bootstrapFile string
		containerfile string
		packages      []string
		ansibleCore   string
		memoryMax     string
		cpuQuota      string
	)
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a runner definition (" + deferredNote + ")",
		Long: "Model A is --base with --bootstrap-file (a script run as one layer) and/or " +
			"--packages; Model B is --base with --containerfile (a path). The two are " +
			"mutually exclusive. Both feed the same content hash, so switching --mode never " +
			"changes a runner's identity.\n\nThe definition is stored, not built: it is " +
			deferredNote + ".",
		Example: "exocomp runner add fedora-ee --base registry.fedoraproject.org/fedora-minimal:44 --packages sshpass,rsync",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if bootstrapFile != "" && containerfile != "" {
				return fmt.Errorf("runner add: give at most one of --bootstrap-file or --containerfile")
			}
			var bootstrap string
			if bootstrapFile != "" {
				body, err := os.ReadFile(bootstrapFile)
				if err != nil {
					return fmt.Errorf("runner add: read bootstrap: %w", err)
				}
				bootstrap = string(body)
			}
			// design §2: naming a Containerfile selects on-the-fly semantics by
			// default, while an explicit --mode still wins.
			if containerfile != "" && !cmd.Flags().Changed("mode") {
				mode = store.RunnerModeOnTheFly
			}

			r := store.Runner{
				Name: args[0], Mode: mode, Base: base, Bootstrap: bootstrap,
				Containerfile: containerfile, Packages: packages, AnsibleCore: ansibleCore,
				MemoryMax: memoryMax, CPUQuota: cpuQuota,
			}
			if err := validateRunner(r); err != nil {
				return fmt.Errorf("runner add: %w", err)
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			created, err := st.CreateRunner(cmd.Context(), r)
			if err != nil {
				return fmt.Errorf("runner add: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "registered %s runner %q (%s) hash %s\n",
				created.Mode, created.Name, created.Base, created.ContentHash)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), deferredNote)
			return nil
		},
	}
	add.Flags().StringVar(&mode, "mode", store.RunnerModeCached,
		"build model: cached (build once, reuse) or onthefly (rebuild per run)")
	add.Flags().StringVar(&base, "base", "", "base image reference (required)")
	add.Flags().StringVar(&bootstrapFile, "bootstrap-file", "",
		"file whose contents are the Model A bootstrap script; excludes --containerfile")
	add.Flags().StringVar(&containerfile, "containerfile", "",
		"Containerfile path for Model B; excludes --bootstrap-file")
	add.Flags().StringSliceVar(&packages, "packages", nil,
		"system packages to bake in, comma-separated; stored sorted and deduplicated")
	add.Flags().StringVar(&ansibleCore, "ansible-core", "", "ansible-core version pin for the image")
	add.Flags().StringVar(&memoryMax, "memory-max", "", "memory ceiling a run would ask for, e.g. 2G")
	add.Flags().StringVar(&cpuQuota, "cpu-quota", "", "CPU ceiling a run would ask for, e.g. 200%")
	return add
}

func newRunnerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List registered runner definitions",
		Example: "exocomp runner list --output json",
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

			rows, err := st.ListRunners(cmd.Context())
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindRunnerList, wire.NewRunnerList(rows))
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no runners")
				return nil
			}
			for _, r := range rows {
				_, _ = fmt.Fprintf(out, "%-20s %-9s %-44s %-13s %s\n",
					r.Name, r.Mode, r.Base, short(r.ContentHash), r.CreatedAt.Format(time.RFC3339))
			}
			return nil
		},
	}
}

func newRunnerShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "show <name>",
		Short:   "Show one runner definition in full",
		Example: "exocomp runner show fedora-ee",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			r, err := st.RunnerByName(cmd.Context(), args[0])
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return fmt.Errorf("runner show: %w", err)
			}
			if mode == outputJSON {
				// The JSON projection stays body-free in both directions; the
				// bootstrap script is printed below, on the text path only.
				return emitJSON(cmd, wire.KindRunner, wire.NewRunnerView(r))
			}
			printRunner(cmd, r)
			return nil
		},
	}
}

func printRunner(cmd *cobra.Command, r store.Runner) {
	out := cmd.OutOrStdout()
	p := func(label, value string) {
		if value != "" {
			_, _ = fmt.Fprintf(out, "%-14s %s\n", label+":", value)
		}
	}
	p("name", r.Name)
	p("mode", r.Mode)
	p("base", r.Base)
	p("packages", strings.Join(r.Packages, " "))
	p("ansible-core", r.AnsibleCore)
	p("containerfile", r.Containerfile)
	p("memory-max", r.MemoryMax)
	p("cpu-quota", r.CPUQuota)
	p("hash", r.ContentHash)
	p("created", r.CreatedAt.Format(time.RFC3339))
	p("updated", r.UpdatedAt.Format(time.RFC3339))
	p("status", deferredNote)
	if r.Bootstrap != "" {
		_, _ = fmt.Fprintf(out, "%-14s %d lines, sha256 %s\n",
			"bootstrap:", r.BootstrapLines(), r.BootstrapSHA256())
		for _, line := range strings.Split(strings.TrimRight(r.Bootstrap, "\n"), "\n") {
			_, _ = fmt.Fprintf(out, "  %s\n", line)
		}
	}
}

func newRunnerRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a runner definition",
		// No confirmation gate: a definition holds no secret material and no
		// state, nothing executes it, and re-adding it restores the identical
		// content hash. The destructive-confirm contract is for the verbs that
		// destroy something unrecoverable.
		Example: "exocomp runner rm fedora-ee",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err := st.DeleteRunner(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("runner rm: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed runner %q\n", args[0])
			return nil
		},
	}
}

// runnerSpecYAML is the design §2 runner.yaml shape.
//
// build.no_cache is declared but not stored: it is a knob for the build tier,
// and that tier is deferred. Declaring it keeps a spec that carries it
// importable (strict decoding would otherwise refuse the whole file) while the
// import reports that the field had no effect.
type runnerSpecYAML struct {
	Name          string   `yaml:"name"`
	Mode          string   `yaml:"mode"`
	Base          string   `yaml:"base"`
	Bootstrap     string   `yaml:"bootstrap"`
	Containerfile string   `yaml:"containerfile"`
	Packages      []string `yaml:"packages"`
	AnsibleCore   string   `yaml:"ansible_core"`
	Build         struct {
		NoCache bool `yaml:"no_cache"`
	} `yaml:"build"`
	Caps struct {
		MemoryMax string `yaml:"memory_max"`
		CPUQuota  string `yaml:"cpu_quota"`
	} `yaml:"caps"`
}

// parseRunnerSpec decodes one runner.yaml. Both shapes the design shows are
// accepted: the repo-side file wraps the spec under a `runner:` key, while a
// file written for the registry may carry the fields at the top level.
func parseRunnerSpec(data []byte) (runnerSpecYAML, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return runnerSpecYAML{}, err
	}
	node := &doc
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return runnerSpecYAML{}, fmt.Errorf("empty document")
		}
		node = node.Content[0]
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == "runner" {
				node = node.Content[i+1]
				break
			}
		}
	}

	// Re-encode the selected node so it can go through a strict decoder: a
	// typo'd key is an operator mistake worth failing on, not a field silently
	// dropped from the stored definition.
	inner, err := yaml.Marshal(node)
	if err != nil {
		return runnerSpecYAML{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(inner))
	dec.KnownFields(true)
	var spec runnerSpecYAML
	if err := dec.Decode(&spec); err != nil {
		return runnerSpecYAML{}, err
	}
	return spec, nil
}

// runnerFromSpec turns a parsed spec into a store row, applying the same
// defaults `runner add` does.
func runnerFromSpec(spec runnerSpecYAML, fallbackName string) store.Runner {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = fallbackName
	}
	mode := spec.Mode
	if mode == "" {
		mode = store.RunnerModeCached
		if spec.Containerfile != "" {
			mode = store.RunnerModeOnTheFly
		}
	}
	return store.Runner{
		Name: name, Mode: mode, Base: spec.Base, Bootstrap: spec.Bootstrap,
		Containerfile: spec.Containerfile, Packages: spec.Packages,
		AnsibleCore: spec.AnsibleCore,
		MemoryMax:   spec.Caps.MemoryMax, CPUQuota: spec.Caps.CPUQuota,
	}
}

func newRunnerImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <dir>",
		Short: "Load runner.yaml specs from a directory into the registry",
		Long: "Reads every *.yaml/*.yml in the directory as a runner spec (with or without " +
			"the `runner:` wrapper) and creates or updates the runner of that name. This is " +
			"the GitOps ergonomic without splitting state out of the data dir: the database " +
			"stays the one registry, and it is what gets backed up.\n\nA malformed or " +
			"invalid spec fails the import at that file, before any later file is applied.",
		Example: "exocomp runner import ./runners",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			entries, err := os.ReadDir(dir)
			if err != nil {
				return fmt.Errorf("runner import: %w", err)
			}
			var files []string
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if ext := filepath.Ext(e.Name()); ext == ".yaml" || ext == ".yml" {
					files = append(files, e.Name())
				}
			}
			sort.Strings(files)

			out := cmd.OutOrStdout()
			if len(files) == 0 {
				_, _ = fmt.Fprintf(out, "no runner specs in %s\n", dir)
				return nil
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			for _, file := range files {
				data, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					return fmt.Errorf("runner import: %w", err)
				}
				spec, err := parseRunnerSpec(data)
				if err != nil {
					return fmt.Errorf("runner import: %s: %w", file, err)
				}
				r := runnerFromSpec(spec, strings.TrimSuffix(file, filepath.Ext(file)))
				if err := validateRunner(r); err != nil {
					return fmt.Errorf("runner import: %s: %w", file, err)
				}

				verb := "created"
				saved, err := st.CreateRunner(cmd.Context(), r)
				if err != nil {
					if !errors.Is(err, store.ErrDuplicate) {
						return fmt.Errorf("runner import: %s: %w", file, err)
					}
					verb = "updated"
					if saved, err = st.UpdateRunner(cmd.Context(), r); err != nil {
						return fmt.Errorf("runner import: %s: %w", file, err)
					}
				}
				_, _ = fmt.Fprintf(out, "%s %s hash %s (%s)\n", verb, saved.Name, saved.ContentHash, file)
				if spec.Build.NoCache {
					_, _ = fmt.Fprintf(out, "  build.no_cache ignored: %s\n", deferredNote)
				}
			}
			_, _ = fmt.Fprintln(out, deferredNote)
			return nil
		},
	}
}
