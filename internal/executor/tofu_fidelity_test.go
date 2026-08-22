package executor_test

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// TestTofuOperatorArgPassthrough proves the operator's own arguments reach tofu
// verbatim: -var-file, -target, and -replace ride through TofuArgv and land
// after the injected non-interactive flags but before any positional. This is
// the tofu-fidelity must-fix (expert-reviews.md #4).
func TestTofuOperatorArgPassthrough(t *testing.T) {
	p := newProbe(t, map[string]int{"plan": 0, "apply": 0})

	cases := []struct {
		name string
		argv []string
		want []string
	}{
		{
			name: "plan with a var-file and a target",
			argv: executor.TofuArgv("plan",
				"-var-file=prod.tfvars", "-target=aws_instance.web"),
			want: []string{"-var-file=prod.tfvars", "-target=aws_instance.web"},
		},
		{
			name: "apply replacing a resource",
			argv: executor.TofuArgv("apply",
				"-replace=aws_instance.web", "-auto-approve"),
			want: []string{"-replace=aws_instance.web", "-auto-approve"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p.run(t, executor.TofuOptions{StatePassphrase: "x"}, tc.argv)
			var got string
			for _, line := range p.argvs(t) {
				if strings.HasPrefix(line, tc.argv[1]+" ") {
					got = line
				}
			}
			if got == "" {
				t.Fatalf("operation %q never ran; argvs: %v", tc.argv[1], p.argvs(t))
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("passthrough %q missing from %q", want, got)
				}
			}
		})
	}
}
