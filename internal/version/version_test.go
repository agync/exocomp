package version

import "testing"

func TestString(t *testing.T) {
	Version, Commit, Date = "1.2.3", "abc123", "2026-08-21"
	defer func() { Version, Commit, Date = "dev", "dev", "dev" }()

	got := String()
	want := "1.2.3 (abc123) built 2026-08-21"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
