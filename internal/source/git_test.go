package source_test

import (
	"testing"

	"exocomp.dev/exocomp/internal/source"
)

func TestLooksLikeGitURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want bool
	}{
		// scp-like syntax with a login prefix — the canonical case.
		{"git@forge:org/repo.git", true},
		{"git@forge:org/repo", true},
		{"user@host.example:path/to/repo", true},

		// explicit schemes.
		{"ssh://git@forge:2222/org/repo.git", true},
		{"ssh://forge/org/repo.git", true},
		{"https://forge.example/org/repo.git", true},
		{"http://forge.example/org/repo.git", true},
		{"git://forge.example/repo.git", true},
		{"ftp://forge.example/repo.git", true},
		{"ftps://forge.example/repo.git", true},
		{"HTTPS://Forge.Example/org/repo.git", true}, // scheme match is case-insensitive
		{"file:///data/repo", false},                 // file:// is not in the recognized scheme list

		// scp-like syntax with no login, dot-shaped or localhost host.
		{"host.example:path/to/repo", true},
		{"localhost:path/to/repo", true},

		// filesystem paths — never match.
		{"/abs/path", false},
		{"/abs/path/to/repo.git", false},
		{"relative/path", false},
		{"./x", false},
		{"../x", false},
		{"../../etc", false},
		{"bare-name", false},
		{"", false},
		{"   ", false},

		// ambiguous / edge cases.
		{"a:relative", false},  // single-char host-like token before colon: not host-shaped
		{"x@y:relative", true}, // login prefix wins even with a short, dot-less host
		{"notes.txt:backup", true},
	}
	for _, tc := range cases {
		if got := source.LooksLikeGitURL(tc.raw); got != tc.want {
			t.Errorf("LooksLikeGitURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
