package packaging

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// hostKeyPassphrasePath is the one path the unit and its documentation share.
const hostKeyPassphrasePath = ".config/exocomp/host-key.passphrase"

// quadletSections splits the unit so an assertion can name the section a key
// belongs to: LoadCredential= is a service key and Environment= is a container
// key, and a line in the wrong section is a unit that will not start.
func quadletSections(t *testing.T) (container, service string) {
	t.Helper()
	body := readPackagingFile(t, "quadlet/exocomp.container")
	section := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.Trim(trimmed, "[]")
			continue
		}
		switch section {
		case "Container":
			container += line + "\n"
		case "Service":
			service += line + "\n"
		}
	}
	if container == "" || service == "" {
		t.Fatal("the quadlet has no [Container] or no [Service] section")
	}
	return container, service
}

// TestQuadletPassesTheHostKeyPassphrase: a systemd user manager cannot decrypt a
// host-scope credential, so the rootless unit uses the passphrase tier. The
// wrapped key stays in the volume and only the passphrase, delivered as a plain
// credential, lives outside it; a stolen data volume therefore lacks the key.
func TestQuadletPassesTheHostKeyPassphrase(t *testing.T) {
	container, service := quadletSections(t)

	if strings.Contains(container, "LoadCredential") {
		t.Error("a credential key is in [Container]; the generator rejects it there")
	}
	if strings.Contains(service, "LoadCredentialEncrypted=") {
		t.Error("the quadlet uses LoadCredentialEncrypted=, which a user manager cannot " +
			"decrypt; the unit will not start")
	}
	if !strings.Contains(service, "LoadCredential=passphrase:%h/"+hostKeyPassphrasePath) {
		t.Errorf("the passphrase is not loaded as a service credential from %%h/%s", hostKeyPassphrasePath)
	}
	for _, want := range []string{
		"Environment=EXOCOMP_HOST_KEY_PASSPHRASE_FILE=/run/credentials/exocomp.service/passphrase",
		"Environment=CREDENTIALS_DIRECTORY=/run/credentials/exocomp.service",
	} {
		if !strings.Contains(container, want) {
			t.Errorf("[Container] does not contain %q; the container cannot read the passphrase", want)
		}
	}
	if !strings.Contains(container, "Volume=%t/credentials/exocomp.service:/run/credentials/exocomp.service:ro,Z") {
		t.Error("[Container] does not mount the service's credential directory")
	}
}

// TestBootstrapDocDocumentsThePassphraseTier: the host-key-at-rest operation is
// documented rather than scripted, and for the rootless unit it is the
// passphrase tier — the passphrase file sits outside the data volume while the
// wrapped key stays inside it. The doc is the contract, so it must carry the
// same path the unit reads and the reason the systemd-credential tier is not
// used here.
func TestBootstrapDocDocumentsThePassphraseTier(t *testing.T) {
	body := readPackagingFile(t, "../docs/operations/bootstrap.md")

	for _, want := range []string{
		"host.key.age",
		"EXOCOMP_HOST_KEY_PASSPHRASE_FILE",
		hostKeyPassphrasePath,
		"Scope mismatch",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bootstrap.md does not name %q; the passphrase tier is undocumented", want)
		}
	}
}

// TestComposeMountsTheHostKeyPassphrase: the compose file must bind-mount the
// host-key passphrase from outside the volume, with the shared SELinux relabel
// (`z`) a file mount needs and the read-only flag, so the wrapped key in the
// volume is useless without the host file. It must not go back to a Compose
// file secret: that is a plain bind mount without a relabel, which a non-root
// container cannot read on an SELinux host.
func TestComposeMountsTheHostKeyPassphrase(t *testing.T) {
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			Volumes     []string          `yaml:"volumes"`
		} `yaml:"services"`
		Secrets map[string]any `yaml:"secrets"`
	}
	if err := yaml.Unmarshal([]byte(readPackagingFile(t, "compose.yaml")), &doc); err != nil {
		t.Fatalf("parse compose.yaml: %v", err)
	}
	if len(doc.Secrets) != 0 {
		t.Error("compose.yaml declares secrets again; the passphrase is a bind mount with a relabel")
	}
	svc, ok := doc.Services["exocomp"]
	if !ok {
		t.Fatal("compose.yaml has no exocomp service")
	}
	var mount string
	for _, v := range svc.Volumes {
		if strings.Contains(v, "/run/secrets/host-key.passphrase") {
			mount = v
			break
		}
	}
	if mount == "" {
		t.Fatal("compose.yaml does not mount the host-key passphrase")
	}
	if !strings.Contains(mount, "${EXOCOMP_HOST_KEY_PASSPHRASE_FILE:?") {
		t.Errorf("the passphrase mount does not require EXOCOMP_HOST_KEY_PASSPHRASE_FILE: %q", mount)
	}
	parts := strings.Split(mount, ":")
	opts := strings.Split(parts[len(parts)-1], ",")
	has := func(want string) bool {
		for _, o := range opts {
			if o == want {
				return true
			}
		}
		return false
	}
	if !has("ro") || !has("z") {
		t.Errorf("the passphrase mount is not read-only with the shared SELinux relabel: %q", mount)
	}
	if got := svc.Environment["EXOCOMP_HOST_KEY_PASSPHRASE_FILE"]; got != "/run/secrets/host-key.passphrase" {
		t.Errorf("EXOCOMP_HOST_KEY_PASSPHRASE_FILE = %q, want the mounted path", got)
	}
}
