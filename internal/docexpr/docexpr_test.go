package docexpr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsExamples is the guard: every CEL example in the bundle must compile
// against the environment its verb uses, and an event expression must name only
// fields a normalized event carries. This is the test that would have caught the
// `event.payload` and `event.status == 'down'` examples that shipped in the
// guides.
func TestDocsExamples(t *testing.T) {
	findings, err := Check(filepath.Join("..", "..", "docs"))
	if err != nil {
		t.Fatalf("scan docs: %v", err)
	}
	for _, f := range findings {
		t.Errorf("stale doc expression: %s", f)
	}
}

// TestCheck pins the extractor and each rule against fixtures, so a regression
// in the guard itself fails here rather than silently letting docs rot.
func TestCheck(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string // one expected finding substring per finding
	}{
		{
			name: "valid event and plan examples",
			doc: "--expr \"event.name == 'x' && event.labels['g'] == 'home'\"\n" +
				"--expr 'plan.resource_changes.size() > 0'\n",
		},
		{
			name: "placeholder is skipped",
			doc:  "exocomp rule add n --expr '...' --action playbook\n",
		},
		{
			name: "unquoted flag spelling is skipped",
			doc:  "  --expr event   CEL expression over event\n",
		},
		{
			name: "prose is not an expression",
			doc:  "See internal/reactive/event.go:17 and event.check_observations_json.\n",
		},
		{
			name: "unknown event field",
			doc:  "--expr \"event.payload.alertname == 'DiskPressure'\"\n",
			want: []string{"event.payload is not a field"},
		},
		{
			name: "unknown status literal",
			doc:  "--expr \"event.status == 'down'\"\n",
			want: []string{"only firing or resolved"},
		},
		{
			name: "event expression that will not compile",
			doc:  "--expr \"event.name ==\"\n",
			want: []string{"compile"},
		},
		{
			name: "plan expression that will not compile",
			doc:  "--expr 'plan. == 1'\n",
			want: []string{"compile"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "page.md"), []byte(tc.doc), 0o644); err != nil {
				t.Fatal(err)
			}
			findings, err := Check(dir)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if len(findings) != len(tc.want) {
				t.Fatalf("got %d findings %v, want %d", len(findings), findings, len(tc.want))
			}
			for i, want := range tc.want {
				if !strings.Contains(findings[i].Problem, want) {
					t.Errorf("finding %d = %q, want substring %q", i, findings[i].Problem, want)
				}
			}
		})
	}
}
