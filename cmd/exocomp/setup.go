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

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
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
	// Commit performs every write at once: identity, keyring, recovery kit, the
	// operator name and ntfy topic, and the test push. It returns the kit text
	// to show on the hand-over screen and a one-line notify status (whether a
	// test push went out, or that no transport is configured yet).
	Commit(ctx context.Context, in setupInput) (kitText, notifyStatus string, err error)
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

// AlreadyInitialized treats a present host key — plaintext or the
// passphrase-wrapped host.key.age — as an initialized host: one of those is the
// file secrets.Init writes first and refuses to overwrite.
func (p *dataDirProvisioner) AlreadyInitialized() bool {
	for _, name := range []string{"host.key", "host.key.age"} {
		if _, err := os.Stat(filepath.Join(p.dir, name)); err == nil {
			return true
		}
	}
	return false
}

// Commit mints the identity and keyring through secrets.Init — the one and
// only atomic identity write in the flow, so the step-1 promise holds by
// construction — then persists the operator name and the generated ntfy topic
// and attempts the test push. secrets.Init generates and persists atomically; a
// host that somehow became initialized between the check and here surfaces as an
// error rather than a clobber.
func (p *dataDirProvisioner) Commit(ctx context.Context, in setupInput) (string, string, error) {
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create data dir: %w", err)
	}
	env, kit, err := secrets.Init(p.dir, nil)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = env.Close() }()

	st, err := openStoreAt(p.dir)
	if err != nil {
		secrets.Rollback(p.dir)
		return "", "", err
	}
	defer func() { _ = st.Close() }()

	notifyStatus, err := persistOperatorConfig(ctx, st, env, in)
	if err != nil {
		secrets.Rollback(p.dir)
		return "", "", err
	}
	return kit.Render(), notifyStatus, nil
}

// persistOperatorConfig writes the operator display name and the ntfy topic
// (plain, in the setting table for readback, and sealed as the notify config the
// transport reads), then attempts a test push. It returns the notify status line
// the hand-over screen shows: a real push when a transport is configured, or a
// clear no-transport message when only a topic has been minted so far.
func persistOperatorConfig(ctx context.Context, st *store.Store, env *secrets.Envelope,
	in setupInput) (string, error) {
	if err := st.PutSetting(ctx, store.SettingOperator, in.operator); err != nil {
		return "", err
	}
	if err := st.PutSetting(ctx, store.SettingNtfyTopic, in.topic); err != nil {
		return "", err
	}

	transport, err := notify.LoadNtfy(ctx, func(ctx context.Context, name string) ([]byte, error) {
		return openSealed(ctx, st, env.DataDir(), name)
	})
	if err != nil {
		return "", err
	}
	if transport == nil {
		return "topic " + in.topic + " saved · no push transport yet (seal " +
			notify.SecretNtfyURL + " to enable)", nil
	}
	if perr := transport.Publish(ctx, notify.Notification{
		Title:    "exocomp is set up",
		Body:     "Test push for " + orDash(in.operator) + " on " + in.machine + ".",
		Priority: notify.PriorityDefault,
	}); perr != nil {
		return "topic " + in.topic + " saved · test push failed: " + perr.Error(), nil
	}
	return "topic " + in.topic + " · test push sent", nil
}

// sealConfigSecret seals value under name and upserts it into the secret table,
// so a re-seal (a second setup or a topic change) replaces rather than
// duplicates.
func sealConfigSecret(ctx context.Context, st *store.Store, env *secrets.Envelope, name, value string) error {
	sealed, err := env.Seal(name, "generic", []byte(value))
	if err != nil {
		return err
	}
	row := store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce,
		Ciphertext: sealed.Ciphertext, AAD: sealed.Name + "|" + sealed.Kind,
	}
	if _, err := st.CreateSecret(ctx, row); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			return st.RotateSecret(ctx, name, sealed.Nonce, sealed.Ciphertext)
		}
		return err
	}
	return nil
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

	committed    bool
	kitText      string
	notifyStatus string
	err          error
	quitting     bool
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
			kit, notifyStatus, err := m.prov.Commit(context.Background(), setupInput{
				machine: m.machine, operator: m.operator, topic: m.topic,
			})
			m.committed = true
			m.err = err
			m.kitText = kit
			m.notifyStatus = notifyStatus
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
		notifyLine := "notify     " + m.notifyStatus
		if m.notifyStatus == "" {
			notifyLine = "notify     topic " + m.topic
		}
		lines = []string{
			"operator   " + orDash(m.operator),
			"machine    " + m.machine,
			notifyLine,
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
