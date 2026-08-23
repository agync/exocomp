package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// hosts.toml is the client-side, SSH-known-hosts-style record of remote daemons
// `exocomp login` has pinned: for each named remote, where it is, the pinned
// cert fingerprint, and the bearer token. It lives outside the data dir because
// it is a workstation (lrrr) artifact, not daemon state, and it holds a token,
// so it is written 0600 like ~/.ssh/known_hosts + a credential.
//
// The file format:
//
//	default = "nibbler"
//	[hosts.nibbler]
//	endpoint    = "https://nibbler.tailnet.ts.net:8443"
//	fingerprint = "AA:BB:...:99"
//	token       = "exo_..."

const hostsFileName = "hosts.toml"

// hostEntry is one pinned remote.
type hostEntry struct {
	Endpoint    string `toml:"endpoint"`
	Fingerprint string `toml:"fingerprint"`
	Token       string `toml:"token"`
}

// hostsConfig is the whole file: a default remote name and the named entries.
type hostsConfig struct {
	Default string               `toml:"default,omitempty"`
	Hosts   map[string]hostEntry `toml:"hosts"`
}

// configDir resolves ~/.config/exocomp, honoring XDG_CONFIG_HOME. It mirrors
// dataDir's discipline but for config, not data — hosts.toml is neither state
// nor a secret store, it is client configuration.
func configDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "exocomp"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "exocomp"), nil
}

func hostsPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hostsFileName), nil
}

// loadHostsConfig reads hosts.toml. A missing file is not an error: it yields an
// empty config, so a fresh workstation with no logins behaves as pure local mode
// rather than failing every command.
func loadHostsConfig() (hostsConfig, error) {
	path, err := hostsPath()
	if err != nil {
		return hostsConfig{}, err
	}
	var cfg hostsConfig
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hostsConfig{Hosts: map[string]hostEntry{}}, nil
		}
		return hostsConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	if cfg.Hosts == nil {
		cfg.Hosts = map[string]hostEntry{}
	}
	return cfg, nil
}

// saveHostsConfig writes hosts.toml atomically at 0600. It writes to a temp file
// in the same dir and renames, so a crash never leaves a half-written file that
// would lock the operator out of every remote. The parent dir is 0700.
func saveHostsConfig(cfg hostsConfig) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	path := filepath.Join(dir, hostsFileName)

	tmp, err := os.CreateTemp(dir, hostsFileName+".*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	// A token lives in this file: lock it down before any bytes land.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := toml.NewEncoder(tmp).Encode(cfg); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install config: %w", err)
	}
	return nil
}
