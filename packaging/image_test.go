package packaging

import (
	"os"
	"strings"
	"testing"
)

// dataDirEnv is what cmd/exocomp resolves its data dir from. The value has to
// be the parent of the mount, because the binary appends "exocomp" to it.
const dataDirEnv = "XDG_DATA_HOME=/var/lib"

func readPackagingFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// TestContainerfileRunsTheDaemon: the entrypoint is the CLI, whose default
// subcommand is nothing. Without a CMD the container starts, prints the help,
// and exits — a control plane that never ran.
func TestContainerfileRunsTheDaemon(t *testing.T) {
	body := readPackagingFile(t, "Containerfile")

	if !strings.Contains(body, `CMD ["serve"]`) {
		t.Error("Containerfile names no default subcommand; the image would print help and exit")
	}
	if !strings.Contains(body, "ENV "+dataDirEnv) {
		t.Errorf("Containerfile does not point %s at the volume's parent", dataDirEnv)
	}
	if !strings.Contains(body, `VOLUME ["/var/lib/exocomp"]`) {
		t.Error("Containerfile no longer declares the data volume the env above assumes")
	}
}

// TestQuadletNamesTheSubcommand: the unit says what it runs rather than
// inheriting it, and points the daemon at the volume it mounts.
func TestQuadletNamesTheSubcommand(t *testing.T) {
	body := readPackagingFile(t, "quadlet/exocomp.container")

	if !strings.Contains(body, "\nExec=serve") {
		t.Error("the quadlet does not name the subcommand it runs")
	}
	if !strings.Contains(body, "Environment="+dataDirEnv) {
		t.Errorf("the quadlet does not set %s, so the daemon writes outside the volume", dataDirEnv)
	}
	if !strings.Contains(body, "Volume=exocomp.volume:/var/lib/exocomp") {
		t.Error("the quadlet no longer mounts the data volume the env above assumes")
	}
}
