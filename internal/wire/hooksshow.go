package wire

// KindHooksShow is the kind of a source's resolved hooks + gate lifecycle.
const KindHooksShow = "hooks.show"

// HookStepView projects one pre/post-run hook step: its name, what it runs (a
// command or a script path — never both), and the secret NAME it injects, if
// any. Like wire.SecretView elsewhere, only the reference travels; the
// secret's value never does.
type HookStepView struct {
	Name   string `json:"name"`
	Run    string `json:"run,omitempty"`
	Script string `json:"script,omitempty"`
	Secret string `json:"secret,omitempty"`
}

// GateProbeView projects one post-run health-gate probe: its name and its
// kind (http/dns/tls/metric). The probe's own target (a URL, a DNS name) is
// left out — the text report never prints it either.
type GateProbeView struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// HooksView is a source's resolved lifecycle: which file is in effect (empty
// when the source has none configured), the ordered pre/post-run steps, and
// the gate.
type HooksView struct {
	Source    string          `json:"source"`
	HooksPath string          `json:"hooks_path,omitempty"`
	Override  bool            `json:"override,omitempty"`
	PreRun    []HookStepView  `json:"pre_run"`
	PostRun   []HookStepView  `json:"post_run"`
	Gate      []GateProbeView `json:"gate"`
}
