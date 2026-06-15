// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package storagesupervisor installs and (eventually) supervises the
// unbounded-storage daemon on a host. The install workflow is a native Go port
// of hack/scripts/install-unbounded-storage.sh: it acquires a release-layout
// tarball (from a GitHub release, an explicit URL, or a local file), verifies
// it, stages it into a versioned prefix, flips a "current" symlink, writes a
// systemd unit, and enables/starts the service via systemctl.
package storagesupervisor

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Default configuration values. These mirror the defaults in
// hack/scripts/install-unbounded-storage.sh so the Go port is drop-in
// compatible with the shell installer's environment interface.
const (
	defaultRepo        = "Azure/unbounded-kube"
	defaultVersion     = "latest"
	defaultPrefix      = "/opt/unbounded-storage"
	defaultServiceName = "unbounded-storage"
	defaultConfigPath  = "/etc/unbounded-storage/config.toml"
	defaultSystemctl   = "systemctl"
	defaultHostRoot    = "/"

	// defaultPoolBytes is the buffer-pool size (bytes) the hugepage reservation
	// is sized for. Matches the daemon's default 128 MiB per-shard backing.
	defaultPoolBytes = 134217728
)

// SourceMode classifies where the release-layout tarball comes from.
type SourceMode int

const (
	// SourceRelease downloads the tarball from a GitHub release (latest or a
	// pinned tag) when no explicit source is provided.
	SourceRelease SourceMode = iota
	// SourceURL downloads the tarball from an explicit http(s) URL.
	SourceURL
	// SourceFile uses a release-layout tarball already present on disk.
	SourceFile
)

// Config holds the resolved configuration for an install run.
type Config struct {
	// Repo is the GitHub "owner/name" used to build release download URLs.
	Repo string
	// Version is the release tag to install, or "latest".
	Version string
	// Prefix is the host-absolute install prefix (e.g. /opt/unbounded-storage).
	// Releases are staged under Prefix/releases and Prefix/current is the
	// active symlink. This is always host-absolute; HostRoot is applied
	// separately when writing files.
	Prefix string
	// ServiceName is the systemd service name (without the .service suffix).
	ServiceName string
	// ConfigPath is the host-absolute path to the daemon config file.
	ConfigPath string
	// StorageArgs are extra arguments appended to the daemon ExecStart line.
	StorageArgs string
	// Arch is the normalized target architecture ("amd64" or "arm64").
	Arch string
	// PoolBytes sizes the hugepage reservation in the systemd unit.
	PoolBytes int64
	// Hugepages, when > 0, reserves an explicit number of 2 MiB hugepages
	// instead of deriving the count from PoolBytes.
	Hugepages int64
	// NoEnable, when true, installs the unit but does not enable/start it.
	NoEnable bool

	// Source is the raw source selector: empty (release), an http(s) URL, or a
	// filesystem path.
	Source string
	// SourceMode is the classification of Source.
	SourceMode SourceMode

	// HostRoot is prefixed to every filesystem write so the installer can run
	// against a mounted host filesystem (or a temp dir in tests). It defaults
	// to "/" so writes land at their real host-absolute locations. It never
	// affects path references embedded in the systemd unit or the "current"
	// symlink target, which must remain host-absolute.
	HostRoot string
	// Systemctl is the argv used to invoke systemctl, parsed from the SYSTEMCTL
	// environment variable. In a container this is typically an nsenter wrapper
	// so the calls execute in the host's namespaces.
	Systemctl []string
}

// LoadConfig builds a Config from the process environment, applying the same
// defaults and validation as the shell installer. It returns an error if any
// value is malformed.
func LoadConfig() (Config, error) {
	cfg := Config{
		Repo:        envOr("REPO", defaultRepo),
		Version:     envOr("VERSION", defaultVersion),
		Prefix:      envOr("PREFIX", defaultPrefix),
		ServiceName: envOr("SERVICE_NAME", defaultServiceName),
		ConfigPath:  envOr("CONFIG_PATH", defaultConfigPath),
		StorageArgs: os.Getenv("STORAGE_ARGS"),
		HostRoot:    envOr("HOST_ROOT", defaultHostRoot),
		Systemctl:   strings.Fields(envOr("SYSTEMCTL", defaultSystemctl)),
	}

	// SOURCE takes precedence; LOCAL_TARBALL is honored for backward
	// compatibility with the shell installer when SOURCE is unset.
	cfg.Source = os.Getenv("SOURCE")
	if cfg.Source == "" {
		cfg.Source = os.Getenv("LOCAL_TARBALL")
	}

	cfg.SourceMode = classifySource(cfg.Source)

	cfg.NoEnable = os.Getenv("NO_ENABLE") == "1"

	if len(cfg.Systemctl) == 0 {
		return Config{}, fmt.Errorf("SYSTEMCTL must not be empty")
	}

	arch, err := resolveArch(os.Getenv("ARCH"))
	if err != nil {
		return Config{}, err
	}

	cfg.Arch = arch

	poolBytes, err := parsePoolBytes(envOr("POOL_BYTES", strconv.Itoa(defaultPoolBytes)))
	if err != nil {
		return Config{}, err
	}

	cfg.PoolBytes = poolBytes

	hugepages, err := parseHugepages(os.Getenv("HUGEPAGES"))
	if err != nil {
		return Config{}, err
	}

	cfg.Hugepages = hugepages

	return cfg, nil
}

// classifySource maps a raw source selector to a SourceMode.
func classifySource(source string) SourceMode {
	switch {
	case source == "":
		return SourceRelease
	case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
		return SourceURL
	default:
		return SourceFile
	}
}

// resolveArch normalizes an explicit ARCH override or the host architecture to
// one of the supported Go-style names.
func resolveArch(override string) (string, error) {
	machine := override
	if machine == "" {
		machine = runtime.GOARCH
	}

	switch machine {
	case "amd64", "x86_64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q: set ARCH=amd64|arm64 explicitly", machine)
	}
}

// parsePoolBytes parses and validates POOL_BYTES as a positive integer.
func parsePoolBytes(raw string) (int64, error) {
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("POOL_BYTES must be a positive integer (bytes), got %q", raw)
	}

	return v, nil
}

// parseHugepages parses the optional HUGEPAGES override. An empty value means
// "derive from POOL_BYTES" and yields 0.
func parseHugepages(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}

	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("HUGEPAGES must be a non-negative integer, got %q", raw)
	}

	return v, nil
}

// envOr returns the value of the named environment variable, or def when it is
// unset or empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}

	return def
}
