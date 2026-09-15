// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/internal/agentartifacts"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const maxNodeExporterChecksumManifestSize = 1024 * 1024

type configureNodeExporter struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// ConfigureNodeExporter installs node exporter and its machine-local systemd unit.
func ConfigureNodeExporter(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &configureNodeExporter{log: log, goalState: goalState}
}

func (c *configureNodeExporter) Name() string { return "configure-node-exporter" }

func (c *configureNodeExporter) Do(ctx context.Context) error {
	if !c.goalState.NodeExporter.Enabled {
		return nil
	}

	if err := c.installBinary(ctx); err != nil {
		return err
	}

	machineDir := c.goalState.MachineDir
	if c.goalState.NodeExporter.TLS.Enabled {
		configDir := filepath.Join(machineDir, strings.TrimPrefix(goalstates.NodeExporterConfigDir, "/"))
		if err := ensureWorldExecutableDir(configDir); err != nil {
			return fmt.Errorf("create node exporter config directory: %w", err)
		}

		if err := utilio.WriteFile(
			filepath.Join(machineDir, strings.TrimPrefix(goalstates.NodeExporterWebConfigPath, "/")),
			renderNodeExporterWebConfig(c.goalState.NodeExporter.TLS),
			0o644,
		); err != nil {
			return fmt.Errorf("write node exporter web config: %w", err)
		}
	}

	unitDir := filepath.Join(machineDir, "etc/systemd/system")
	if err := utilio.WriteFile(
		filepath.Join(unitDir, goalstates.NodeExporterServiceUnit),
		renderNodeExporterService(c.goalState.NodeExporter),
		0o644,
	); err != nil {
		return fmt.Errorf("write node exporter service: %w", err)
	}

	if err := enableMachineUnit(machineDir, goalstates.NodeExporterServiceUnit); err != nil {
		return err
	}

	return nil
}

func (c *configureNodeExporter) installBinary(ctx context.Context) error {
	destination := filepath.Join(c.goalState.MachineDir, strings.TrimPrefix(goalstates.NodeExporterBinaryPath, "/"))
	if nodeExporterVersionMatches(ctx, destination, c.goalState.NodeExporter.Version) {
		return nil
	}

	if err := ensureWorldExecutableDir(filepath.Dir(destination)); err != nil {
		return fmt.Errorf("create node exporter binary directory: %w", err)
	}

	override := nodeExporterDownloadSource(c.goalState)
	archiveURL := agentartifacts.NodeExporterArchive(override, c.goalState.NodeExporter.Version, c.goalState.HostArch)

	source, err := artifactsource.Parse(archiveURL)
	if err != nil {
		return fmt.Errorf("resolve node exporter source: %w", err)
	}

	checksumSource, err := artifactsource.Parse(agentartifacts.NodeExporterChecksum(override, c.goalState.NodeExporter.Version, c.goalState.HostArch))
	if err != nil {
		return fmt.Errorf("resolve node exporter checksum source: %w", err)
	}

	archiveName := fmt.Sprintf("node_exporter-%s.linux-%s.tar.gz", strings.TrimPrefix(c.goalState.NodeExporter.Version, "v"), c.goalState.HostArch)

	expectedHash, err := nodeExporterExpectedSHA256(ctx, checksumSource, archiveName)
	if err != nil {
		return fmt.Errorf("read node exporter checksum: %w", err)
	}

	temp, err := os.CreateTemp("", "unbounded-node-exporter-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create node exporter temporary archive: %w", err)
	}

	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close node exporter temporary archive: %w", err)
	}
	defer os.Remove(tempPath) //nolint:errcheck // best effort

	if err := source.DownloadWithSHA256Verification(ctx, expectedHash, tempPath, 0o600); err != nil {
		return fmt.Errorf("download node exporter archive: %w", err)
	}

	archive, err := artifactsource.Parse(tempPath)
	if err != nil {
		return fmt.Errorf("open node exporter archive: %w", err)
	}

	expectedBinary := fmt.Sprintf("node_exporter-%s.linux-%s/node_exporter", strings.TrimPrefix(c.goalState.NodeExporter.Version, "v"), c.goalState.HostArch)
	found := false

	for file, err := range archive.DecompressTarGz(ctx) {
		if err != nil {
			return fmt.Errorf("extract node exporter archive: %w", err)
		}

		if filepath.ToSlash(filepath.Clean(file.Name)) != expectedBinary {
			continue
		}

		if found {
			return fmt.Errorf("node exporter archive contains multiple node_exporter binaries")
		}

		if err := utilio.InstallFile(destination, file.Body, 0o755); err != nil {
			return fmt.Errorf("install node exporter: %w", err)
		}

		found = true
	}

	if !found {
		return fmt.Errorf("node exporter archive does not contain node_exporter")
	}

	if !nodeExporterVersionMatches(ctx, destination, c.goalState.NodeExporter.Version) {
		return fmt.Errorf("installed node exporter version does not match %q", c.goalState.NodeExporter.Version)
	}

	c.log.Info("installed node exporter", "version", c.goalState.NodeExporter.Version)

	return nil
}

func nodeExporterExpectedSHA256(ctx context.Context, source artifactsource.Source, archiveName string) (string, error) {
	reader, err := source.Open(ctx)
	if err != nil {
		return "", err
	}
	defer reader.Close() //nolint:errcheck // best effort

	data, err := io.ReadAll(io.LimitReader(reader, maxNodeExporterChecksumManifestSize+1))
	if err != nil {
		return "", fmt.Errorf("read checksum manifest: %w", err)
	}

	if len(data) > maxNodeExporterChecksumManifestSize {
		return "", fmt.Errorf("checksum manifest exceeds %d bytes", maxNodeExporterChecksumManifestSize)
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 1 {
			return validateNodeExporterChecksum(fields[0], archiveName)
		}

		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archiveName {
			return validateNodeExporterChecksum(fields[0], archiveName)
		}
	}

	return "", fmt.Errorf("checksum manifest does not contain %s", archiveName)
}

func validateNodeExporterChecksum(value, archiveName string) (string, error) {
	if len(value) != 64 {
		return "", fmt.Errorf("checksum for %s is not SHA256", archiveName)
	}

	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("checksum for %s is invalid: %w", archiveName, err)
	}

	return strings.ToLower(value), nil
}

func nodeExporterVersionMatches(ctx context.Context, binaryPath, version string) bool {
	if !utilio.IsExecutable(binaryPath) {
		return false
	}

	var output []byte

	err := executil.RetryWhileTextFileBusy(ctx, slog.Default(), func() error {
		var runErr error

		output, runErr = exec.CommandContext(ctx, binaryPath, "--version").CombinedOutput()

		return runErr
	})

	return err == nil && strings.Contains(string(output), strings.TrimPrefix(version, "v"))
}

func renderNodeExporterService(goal goalstates.NodeExporter) []byte {
	args := []string{"--web.listen-address=" + goal.ListenAddress}
	if goal.TLS.Enabled {
		args = append(args, "--web.config.file="+goalstates.NodeExporterWebConfigPath)
	}

	args = append(args, goal.ExtraArgs...)

	var command strings.Builder
	command.WriteString(goalstates.NodeExporterBinaryPath)

	for _, arg := range args {
		command.WriteByte(' ')
		command.WriteString(systemdQuoteArgument(arg))
	}

	return []byte(`[Unit]
Description=Prometheus Node Exporter
Documentation=https://github.com/prometheus/node_exporter
After=network.target

[Service]
Type=simple
ExecStart=` + command.String() + `
Restart=on-failure
RestartSec=10
NoNewPrivileges=yes
ProtectHome=yes
ProtectSystem=strict
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`)
}

func renderNodeExporterWebConfig(tls goalstates.NodeExporterTLS) []byte {
	var out strings.Builder
	out.WriteString("tls_server_config:\n")
	out.WriteString("  cert_file: \"")
	out.WriteString(yamlDoubleQuoteValue(tls.CertificateFile))
	out.WriteString("\"\n  key_file: \"")
	out.WriteString(yamlDoubleQuoteValue(tls.PrivateKeyFile))
	out.WriteString("\"\n")

	if tls.ClientCAFile == "" {
		out.WriteString("  client_auth_type: NoClientCert\n")
	} else {
		out.WriteString("  client_auth_type: RequireAndVerifyClientCert\n  client_ca_file: \"")
		out.WriteString(yamlDoubleQuoteValue(tls.ClientCAFile))
		out.WriteString("\"\n")
	}

	return []byte(out.String())
}

func yamlDoubleQuoteValue(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}

func systemdQuoteArgument(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`).Replace(value)

	return `"` + value + `"`
}

func nodeExporterDownloadSource(rootFS *goalstates.RootFS) *goalstates.DownloadSource {
	if rootFS.Downloads == nil {
		return nil
	}

	return rootFS.Downloads.NodeExporter
}
