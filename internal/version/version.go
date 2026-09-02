package version

// Set via -ldflags "-X exocomp.dev/exocomp/internal/version.Version=... -X ...Commit=... -X ...Date=..."
var (
	Version = "dev"
	Commit  = "dev"
	Date    = "dev"
)

// String returns the "version (commit) built date" summary used by the CLI.
// A segment equal to the version is omitted (R12): a dev build with all three
// at "dev" reads as just "dev", not "dev (dev) built dev". A real stamp keeps
// the full form.
func String() string {
	s := Version
	if Commit != "" && Commit != Version {
		s += " (" + Commit + ")"
	}
	if Date != "" && Date != Version {
		s += " built " + Date
	}
	return s
}
