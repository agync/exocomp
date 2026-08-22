package litestream

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// ErrUnencryptedRemote is returned when a config would ship WAL frames to a
// remote target Litestream cannot encrypt, without an explicit operator
// acknowledgement. Callers compare against it with errors.Is to distinguish a
// caveat refusal (recoverable with --ack-unencrypted) from a parse error.
var ErrUnencryptedRemote = errors.New("litestream: remote replica target is not encrypted; front it with an encrypted store or LAN-local path, or pass --ack-unencrypted")

// ReplicaType is the litestream replica `type:` value.
type ReplicaType string

// The litestream replica types exocomp emits config for.
const (
	ReplicaFile ReplicaType = "file"
	ReplicaS3   ReplicaType = "s3"
	ReplicaSFTP ReplicaType = "sftp"
)

// Target is a parsed replica destination. It is the single source of truth for
// both config rendering and the encryption caveat: Remote decides whether an
// acknowledgement is required, and the typed fields feed the YAML.
type Target struct {
	Raw  string
	Type ReplicaType

	// File
	Path string

	// S3
	Bucket   string
	Endpoint string
	Region   string

	// SFTP
	Host string
	User string
}

// Remote reports whether the target leaves the LAN. A file replica (a local
// disk or an NFS/SMB mount) is treated as LAN-local and needs no ack; an object
// store or SFTP endpoint crosses the network in the clear and does.
func (t Target) Remote() bool {
	return t.Type == ReplicaS3 || t.Type == ReplicaSFTP
}

// ParseTarget resolves a target URI into a typed Target. A bare path or a
// file:// URL is a local file replica; s3:// and sftp:// are their respective
// remote replicas. Anything else is refused rather than guessed at, so an
// operator never discovers an unsupported scheme after the sidecar is running.
func ParseTarget(raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Target{}, errors.New("litestream: empty replica target")
	}

	// A bare absolute or relative path (no scheme) is a local file replica.
	if !strings.Contains(raw, "://") {
		return Target{Raw: raw, Type: ReplicaFile, Path: raw}, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, fmt.Errorf("litestream: parse target %q: %w", raw, err)
	}

	switch u.Scheme {
	case "file":
		p := u.Path
		if p == "" {
			p = u.Opaque
		}
		if p == "" {
			return Target{}, fmt.Errorf("litestream: file target %q has no path", raw)
		}
		return Target{Raw: raw, Type: ReplicaFile, Path: p}, nil

	case "s3":
		// s3://bucket/prefix — host is the bucket, path is the key prefix.
		// endpoint and region ride as query params for non-AWS stores.
		if u.Host == "" {
			return Target{}, fmt.Errorf("litestream: s3 target %q has no bucket", raw)
		}
		t := Target{
			Raw:      raw,
			Type:     ReplicaS3,
			Bucket:   u.Host,
			Path:     strings.TrimPrefix(u.Path, "/"),
			Endpoint: u.Query().Get("endpoint"),
			Region:   u.Query().Get("region"),
		}
		return t, nil

	case "sftp":
		if u.Host == "" {
			return Target{}, fmt.Errorf("litestream: sftp target %q has no host", raw)
		}
		return Target{
			Raw:  raw,
			Type: ReplicaSFTP,
			Host: u.Host,
			Path: path.Clean("/" + strings.TrimPrefix(u.Path, "/")),
			User: u.User.Username(),
		}, nil

	default:
		return Target{}, fmt.Errorf("litestream: unsupported replica scheme %q (want a path, file://, s3://, or sftp://)", u.Scheme)
	}
}
