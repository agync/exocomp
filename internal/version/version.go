package version

// Set via -ldflags "-X exocomp.dev/exocomp/internal/version.Version=... -X ...Commit=... -X ...Date=..."
var (
	Version = "dev"
	Commit  = "dev"
	Date    = "dev"
)

// String returns the "version (commit) built date" summary used by the CLI.
func String() string {
	return Version + " (" + Commit + ") built " + Date
}
