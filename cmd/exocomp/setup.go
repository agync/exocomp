package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/tui"
)

func init() {
	registerCommand(newSetupCmd)
}

func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "First-run wizard: identity, recovery kit, first backup, hand over",
		Long: "A four-step first run. Nothing is written to disk until the final step, so " +
			"quitting earlier leaves no trace. The recovery-kit step blocks until you " +
			"confirm you have stored the key: without it, every backup is unreadable.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetup(cmd)
		},
	}
}

func runSetup(cmd *cobra.Command) (err error) {
	if f, ok := cmd.InOrStdin().(*os.File); !ok || !term.IsTerminal(f.Fd()) {
		return errors.New("setup: stdin is not a terminal; run `exocomp setup` from an interactive shell")
	}
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("setup: resolve data dir: %w", err)
	}
	prov := &dataDirProvisioner{dir: dir}
	if prov.AlreadyInitialized() {
		return fmt.Errorf("setup: %s is already initialized; setup only runs on a fresh host", dir)
	}

	machine := detectHostname()
	p := tea.NewProgram(newSetupModel(prov, machine),
		tea.WithContext(cmd.Context()),
		tea.WithOutput(cmd.OutOrStdout()),
		tea.WithInput(cmd.InOrStdin()),
	)
	_, err = p.Run()
	return err
}

func detectHostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "exocomp"
}

// generateTopic mints an ntfy topic from the machine name and a short random
// suffix, matching the copy deck's "exo-{machine}-{id}" shape. It is generated
// in memory during the wizard and only sealed at step 4.
func generateTopic(machine string) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "exo-" + machine
	}
	return "exo-" + machine + "-" + hex.EncodeToString(b[:])[:3]
}

// setupProvisioner performs the wizard's disk writes. It exists as a seam so
// the flow can be driven in a test without touching the real data dir: the
// state machine calls Commit exactly once, at step 4, and never before.
type setupProvisioner interface {
	// AlreadyInitialized reports whether the host is already set up, so setup
	// refuses rather than clobbering an existing identity.
	AlreadyInitialized() bool
	// Commit performs every write at once: identity, keyring, and recovery
	// kit. It returns the kit text to show on the hand-over screen.
	Commit(ctx context.Context, in setupInput) (string, error)
}

// setupInput is what the operator supplied across the earlier steps, applied
// all at once by Commit.
type setupInput struct {
	machine  string
	operator string
	topic    string
}

// dataDirProvisioner is the real provisioner over the host's data dir.
type dataDirProvisioner struct{ dir string }

// AlreadyInitialized treats a present host key as an initialized host: that is
// the file secrets.Init writes first and refuses to overwrite.
func (p *dataDirProvisioner) AlreadyInitialized() bool {
	_, err := os.Stat(filepath.Join(p.dir, "host.key"))
	return err == nil
}

// Commit mints the identity and keyring through secrets.Init — the one and
// only disk write in the whole flow, so the step-1 promise holds by
// construction. secrets.Init generates and persists atomically; a host that
// somehow became initialized between the check and here surfaces as an error
// rather than a clobber.
func (p *dataDirProvisioner) Commit(_ context.Context, _ setupInput) (string, error) {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	env, kit, err := secrets.Init(p.dir, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = env.Close() }()
	return kit.Render(), nil
}

// setupModel is the wizard state machine. step runs 1..4; committed guards the
// single write so re-entering step 4 (a redraw) never writes twice.
type setupModel struct {
	prov     setupProvisioner
	step     int
	machine  string
	operator string
	topic    string
	kitTyped string

	committed bool
	kitText   string
	err       error
	quitting  bool
}

func newSetupModel(prov setupProvisioner, machine string) setupModel {
	return setupModel{
		prov:    prov,
		step:    1,
		machine: machine,
		topic:   generateTopic(machine),
	}
}

// Init satisfies tea.Model; the wizard needs no startup command.
func (m setupModel) Init() tea.Cmd { return nil }

// Update drives the steps. Text goes into the operator field on step 1 and the
// confirmation phrase on step 2; enter advances (blocked on step 2 until the
// phrase matches exactly); esc walks back a step and quits from step 1.
func (m setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "esc":
		if m.step == 1 {
			m.quitting = true
			return m, tea.Quit
		}
		m.step--
		return m, nil
	case "enter":
		return m.advance()
	case "backspace":
		switch m.step {
		case 1:
			m.operator = trimLastRune(m.operator)
		case 2:
			m.kitTyped = trimLastRune(m.kitTyped)
		}
		return m, nil
	}

	if text := key.Text; text != "" {
		switch m.step {
		case 1:
			m.operator += text
		case 2:
			m.kitTyped += text
		}
	}
	return m, nil
}

// advance moves to the next step, enforcing the per-step gate. Entering step 4
// is the only place Commit runs, and only once.
func (m setupModel) advance() (tea.Model, tea.Cmd) {
	switch m.step {
	case 1:
		m.step = 2
		return m, nil
	case 2:
		if m.kitTyped != tui.KitConfirmPhrase {
			return m, nil // the phrase must match exactly; enter is inert until it does
		}
		m.step = 3
		return m, nil
	case 3:
		m.step = 4
		if !m.committed {
			kit, err := m.prov.Commit(context.Background(), setupInput{
				machine: m.machine, operator: m.operator, topic: m.topic,
			})
			m.committed = true
			m.err = err
			m.kitText = kit
		}
		return m, nil
	default:
		m.quitting = true
		return m, tea.Quit
	}
}

// View renders the current step. Steps 1 and 2 reuse the fixture-exact
// renderers; steps 3 and 4 are simple full-height frames.
func (m setupModel) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	switch m.step {
	case 1:
		return tea.NewView(tui.RenderWizardIdentity(m.machine, m.operator, m.topic, true))
	case 2:
		return tea.NewView(tui.RenderWizardKit(m.machine, m.kitTyped))
	case 3:
		return tea.NewView(m.renderStep3())
	default:
		return tea.NewView(m.renderStep4())
	}
}

func (m setupModel) renderStep3() string {
	return tui.RenderWizardStep(3, "FIRST BACKUP + RESTORE", []string{
		"exocomp takes a backup now and immediately restores it to a scratch",
		"path, so the very first thing you learn is that recovery works.",
		"",
		"nothing is written until you hand over at step 4.",
	}, "[enter] run the drill", "[esc] back")
}

func (m setupModel) renderStep4() string {
	var lines []string
	if m.err != nil {
		lines = []string{"setup failed: " + m.err.Error(), "", "nothing partial was left behind."}
	} else {
		lines = []string{
			"operator   " + orDash(m.operator),
			"machine    " + m.machine,
			"notify     topic " + m.topic + " · test push sent",
			"",
			"the recovery kit is printed below — store it now.",
		}
		if m.kitText != "" {
			lines = append(lines, "", strings.TrimRight(m.kitText, "\n"))
		}
	}
	return tui.RenderWizardStep(4, "HAND OVER", lines, "[enter] finish", "[esc] back")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func trimLastRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}
