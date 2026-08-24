package naming_test

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/naming"
)

func TestComponent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"dot", ".", false},
		{"dotdot", "..", false},
		{"parent traversal", "../..", false},
		{"nested separator", "a/b/c", false},
		{"leading slash", "/etc/passwd", false},
		{"trailing slash", "name/", false},
		{"backslash", `a\b`, false},
		{"nul byte", "evil\x00name", false},
		{"space", "a b", false},
		{"unicode", "wörkspace", false},
		{"plain word", "prod", true},
		{"dash and underscore", "prod-net_01", true},
		{"embedded dot", "prod.net", true},
		{"leading dot but not dot-only", ".hidden", true},
		{"digits", "12345", true},
		{"at max length", strings.Repeat("a", 200), true},
		{"over max length", strings.Repeat("a", 201), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := naming.Component(tc.in); got != tc.want {
				t.Errorf("Component(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateComponent(t *testing.T) {
	t.Parallel()
	if err := naming.ValidateComponent("workspace", "prod"); err != nil {
		t.Fatalf("ValidateComponent(prod) = %v, want nil", err)
	}
	err := naming.ValidateComponent("workspace", "../..")
	if err == nil {
		t.Fatal("ValidateComponent(../..) = nil, want error")
	}
	if !strings.Contains(err.Error(), "workspace") || !strings.Contains(err.Error(), "../..") {
		t.Errorf("error %q does not name the kind and the offending value", err)
	}
}
