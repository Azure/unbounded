// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// Ignition configuration types, covering only the subset this command emits,
// rather than a dependency carrying the whole specification.
const ignitionSpecVersion = "3.4.0"

// Paths the Ignition variant writes on the target host. The host is a new
// installation, so the host root is a real directory and needs no resolving.
// `unbounded-agent start` reads the config path from UNBOUNDED_AGENT_CONFIG_FILE.
const (
	ignitionAgentConfigPath = "/etc/unbounded/agent/config.json"
	ignitionAgentBinaryPath = hostroot.Path + "/bin/unbounded-agent"
)

type ignitionConfig struct {
	Ignition ignitionVersion  `json:"ignition"`
	Storage  *ignitionStorage `json:"storage,omitempty"`
	Systemd  *ignitionSystemd `json:"systemd,omitempty"`
}

type ignitionVersion struct {
	Version string `json:"version"`
}

type ignitionStorage struct {
	Files []ignitionFile `json:"files,omitempty"`
}

type ignitionFile struct {
	Path      string           `json:"path"`
	Mode      int              `json:"mode,omitempty"`
	Overwrite *bool            `json:"overwrite,omitempty"`
	Contents  ignitionContents `json:"contents"`
}

type ignitionContents struct {
	Source       string                `json:"source"`
	Verification *ignitionVerification `json:"verification,omitempty"`
}

type ignitionVerification struct {
	// Hash is "<algorithm>-<hex>", for example "sha256-abc123...".
	Hash string `json:"hash"`
}

type ignitionSystemd struct {
	Units []ignitionUnit `json:"units,omitempty"`
}

type ignitionUnit struct {
	Name     string `json:"name"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Contents string `json:"contents,omitempty"`
}

// validateIgnitionInput checks the rules that only apply to the Ignition
// variant, before any cluster contact, and normalizes the agent URL and digest.
//
// Every input is required rather than defaulted. Ignition declares state; it
// cannot resolve a version, detect an architecture, or extract an archive at
// boot, so the artifact has to be named exactly, and a host that finds out
// otherwise has no shell and no operator to report it to.
func (h *manualBootstrapHandler) validateIgnitionInput() error {
	h.agentURL = strings.TrimSpace(h.agentURL)
	if h.agentURL == "" {
		return fmt.Errorf("--agent-url is required with --variant %s, and must point at the bare agent binary rather than the release tarball, because Ignition cannot extract an archive", variantIgnition)
	}

	if u, err := url.Parse(h.agentURL); err != nil || !slices.Contains([]string{"http", "https", "tftp", "s3", "arn", "gs"}, u.Scheme) {
		return fmt.Errorf("--agent-url %q cannot be fetched by Ignition; use an http, https, tftp, s3, arn, or gs URL", h.agentURL)
	}

	// A bare digest, or the line for the binary in checksums.txt.
	fields := strings.Fields(h.agentSHA256)
	if len(fields) == 0 {
		return fmt.Errorf("--agent-sha256 is required with --variant %s; the digest for each release binary is published in checksums.txt", variantIgnition)
	}

	// Ignition itself checks only the length, so a bad digest would otherwise
	// fail at first boot.
	if digest, err := hex.DecodeString(fields[0]); err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("invalid --agent-sha256 %q: want 64 hex characters", fields[0])
	}

	h.agentSHA256 = strings.ToLower(fields[0])

	return nil
}

// renderIgnition emits an Ignition config that provisions the host with no
// shell and no operator present. Ignition runs from the initramfs, so the agent
// config, the verified agent binary and the bootstrap unit are all in place
// before any service starts.
func (h *manualBootstrapHandler) renderIgnition(cfg *provision.UnboundedAgentConfig) (string, error) {
	configJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling agent config: %w", err)
	}

	config := ignitionConfig{
		Ignition: ignitionVersion{Version: ignitionSpecVersion},
		Storage: &ignitionStorage{Files: []ignitionFile{
			{
				Path:      ignitionAgentConfigPath,
				Mode:      0o600, // It carries a bootstrap token.
				Overwrite: ptr.To(true),
				Contents:  ignitionContents{Source: "data:;base64," + base64.StdEncoding.EncodeToString(append(configJSON, '\n'))},
			},
			{
				Path:      ignitionAgentBinaryPath,
				Mode:      0o755,
				Overwrite: ptr.To(true),
				Contents: ignitionContents{
					Source:       h.agentURL,
					Verification: &ignitionVerification{Hash: "sha256-" + h.agentSHA256},
				},
			},
		}},
		Systemd: &ignitionSystemd{Units: []ignitionUnit{{
			Name:     provision.FirstBootBootstrapUnit,
			Enabled:  ptr.To(true),
			Contents: fmt.Sprintf(ignitionBootstrapUnit, ignitionAgentBinaryPath, goalstates.DaemonUnit, ignitionAgentConfigPath),
		}}},
	}

	rendered, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling ignition config: %w", err)
	}

	return string(rendered) + "\n", nil
}

// ignitionBootstrapUnit runs the agent from the binary Ignition placed: %[1]s
// is the binary, %[2]s the daemon unit and %[3]s the agent config.
//
// It has no completion condition and runs on every boot, because the agent's
// ownership record already says whether the installation is complete, and a
// marker would be a second record that could disagree with it. On a complete
// host preflight and start return at once, or start repairs a daemon that is
// not running, so a stopped or damaged daemon comes back on reboot. Ordering
// after the daemon unit keeps start from running ahead of the daemon's own
// start job on a reboot, so a daemon systemd is about to start is not repaired.
// The daemon is Type=simple, so this waits only for its process to be started,
// not for it to be ready; start waits for the installation lock the daemon
// holds while it starts up. On first boot the daemon unit does not exist yet.
//
// AssertPathExists, unlike a Condition, fails visibly when Ignition never placed
// the binary. Restart covers DNS that is not answering yet on first boot,
// backing off to five minutes with no start limit, because bootstrap has no
// later chance to run. RestartSteps needs systemd 254; older versions keep the
// fixed RestartSec.
const ignitionBootstrapUnit = `[Unit]
Description=Bootstrap the unbounded agent
Wants=network-online.target
After=network-online.target nss-lookup.target systemd-sysext.service %[2]s
AssertPathExists=%[1]s
StartLimitIntervalSec=0

[Service]
Type=oneshot
RemainAfterExit=yes
Restart=on-failure
RestartSec=10s
RestartSteps=10
RestartMaxDelaySec=300
Environment=UNBOUNDED_AGENT_CONFIG_FILE=%[3]s
ExecStartPre=%[1]s preflight
ExecStart=%[1]s start

[Install]
WantedBy=multi-user.target
`
