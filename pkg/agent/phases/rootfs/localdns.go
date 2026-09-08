// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/internal/agentartifacts"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

//go:embed assets/localdns.service
var localDNSService []byte

//go:embed assets/localdns.slice
var localDNSSliceTemplate string

//go:embed assets/localdns-supervisor.sh
var localDNSSupervisor []byte

type configureLocalDNS struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// ConfigureLocalDNS installs the CoreDNS binary and machine-local service configuration.
func ConfigureLocalDNS(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &configureLocalDNS{log: log, goalState: goalState}
}

func (c *configureLocalDNS) Name() string { return "configure-localdns" }

func (c *configureLocalDNS) Do(ctx context.Context) error {
	if !c.goalState.LocalDNS.Enabled {
		return nil
	}

	if err := c.installCoreDNS(ctx); err != nil {
		return err
	}

	machineDir := c.goalState.MachineDir

	localDNSDir := filepath.Join(machineDir, "etc/unbounded/localdns")
	if err := ensureWorldExecutableDir(localDNSDir); err != nil {
		return fmt.Errorf("create LocalDNS config directory: %w", err)
	}

	if err := utilio.WriteFile(filepath.Join(localDNSDir, "Corefile"), c.goalState.LocalDNS.Corefile, 0o644); err != nil {
		return fmt.Errorf("write LocalDNS Corefile: %w", err)
	}

	if err := utilio.WriteFile(filepath.Join(localDNSDir, "resolv.conf"), localDNSResolvConf(c.goalState.LocalDNS.OriginalHostResolvConf, c.goalState.LocalDNS.NodeListenerIP.String()), 0o644); err != nil {
		return fmt.Errorf("write LocalDNS resolver: %w", err)
	}

	upstreams := make([]string, 0, len(c.goalState.LocalDNS.NodeUpstreamIPs))
	for _, upstream := range c.goalState.LocalDNS.NodeUpstreamIPs {
		upstreams = append(upstreams, upstream.String())
	}

	if err := utilio.WriteFile(filepath.Join(localDNSDir, "node-upstreams"), []byte(strings.Join(upstreams, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("write LocalDNS upstreams: %w", err)
	}

	environment := fmt.Sprintf("NODE_LISTENER=%s\nCLUSTER_LISTENER=%s\n", c.goalState.LocalDNS.NodeListenerIP, c.goalState.LocalDNS.ClusterListenerIP)
	if err := utilio.WriteFile(filepath.Join(localDNSDir, "environment"), []byte(environment), 0o644); err != nil {
		return fmt.Errorf("write LocalDNS environment: %w", err)
	}

	supervisorPath := filepath.Join(machineDir, strings.TrimPrefix(goalstates.LocalDNSSupervisorPath, "/"))
	if err := ensureWorldExecutableDir(filepath.Dir(supervisorPath)); err != nil {
		return fmt.Errorf("create LocalDNS supervisor directory: %w", err)
	}

	if err := utilio.WriteFile(supervisorPath, localDNSSupervisor, 0o755); err != nil {
		return fmt.Errorf("write LocalDNS supervisor: %w", err)
	}

	unitDir := filepath.Join(machineDir, "etc/systemd/system")
	if err := utilio.WriteFile(filepath.Join(unitDir, goalstates.LocalDNSServiceUnit), localDNSService, 0o644); err != nil {
		return fmt.Errorf("write LocalDNS service: %w", err)
	}

	var slice strings.Builder
	if err := template.Must(template.New("localdns-slice").Parse(localDNSSliceTemplate)).Execute(&slice, map[string]any{
		"CPUQuota":  float64(c.goalState.LocalDNS.CPULimitInMilliCores) / 10,
		"MemoryMax": c.goalState.LocalDNS.MemoryLimitInMB,
	}); err != nil {
		return fmt.Errorf("render LocalDNS slice: %w", err)
	}

	if err := utilio.WriteFile(filepath.Join(unitDir, goalstates.LocalDNSSliceUnit), []byte(slice.String()), 0o644); err != nil {
		return fmt.Errorf("write LocalDNS slice: %w", err)
	}

	if err := enableMachineUnit(machineDir, goalstates.LocalDNSServiceUnit); err != nil {
		return err
	}

	for _, unit := range []string{goalstates.SystemdUnitContainerd, goalstates.SystemdUnitKubelet} {
		dropIn := []byte("[Unit]\nRequires=localdns.service\nAfter=localdns.service\n")

		path := filepath.Join(unitDir, unit+".d", "05-localdns.conf")
		if err := utilio.WriteFile(path, dropIn, 0o644); err != nil {
			return fmt.Errorf("write LocalDNS dependency for %s: %w", unit, err)
		}
	}

	return nil
}

func (c *configureLocalDNS) installCoreDNS(ctx context.Context) error {
	destination := filepath.Join(c.goalState.MachineDir, strings.TrimPrefix(goalstates.LocalDNSCoreDNSBinaryPath, "/"))
	if err := ensureWorldExecutableDir(filepath.Dir(destination)); err != nil {
		return fmt.Errorf("create CoreDNS binary directory: %w", err)
	}

	override := coreDNSDownloadSource(c.goalState)

	source, err := artifactsource.Parse(agentartifacts.CoreDNSArchive(override, c.goalState.LocalDNS.CoreDNSVersion, c.goalState.HostArch))
	if err != nil {
		return fmt.Errorf("resolve CoreDNS source: %w", err)
	}

	checksumSource, err := artifactsource.Parse(source.String() + ".sha256")
	if err != nil {
		return fmt.Errorf("resolve CoreDNS checksum source: %w", err)
	}

	expectedHash, err := artifactsource.ReadExpectedSHA256(ctx, checksumSource)
	if err != nil {
		return fmt.Errorf("read CoreDNS checksum: %w", err)
	}

	if strings.Contains(source.String(), ".tgz") {
		temp, err := os.CreateTemp("", "unbounded-coredns-*.tgz")
		if err != nil {
			return fmt.Errorf("create CoreDNS temporary archive: %w", err)
		}

		tempPath := temp.Name()
		if err := temp.Close(); err != nil {
			return fmt.Errorf("close CoreDNS temporary archive: %w", err)
		}

		defer os.Remove(tempPath) //nolint:errcheck // best effort

		if err := source.DownloadWithSHA256Verification(ctx, expectedHash, tempPath, 0o600); err != nil {
			return fmt.Errorf("download CoreDNS archive: %w", err)
		}

		archive, err := artifactsource.Parse(tempPath)
		if err != nil {
			return fmt.Errorf("open CoreDNS archive: %w", err)
		}

		found := false

		for file, err := range archive.DecompressTarGz(ctx) {
			if err != nil {
				return fmt.Errorf("extract CoreDNS archive: %w", err)
			}

			if filepath.Base(file.Name) != "coredns" || strings.Contains(file.Name, "/") {
				continue
			}

			if err := utilio.InstallFile(destination, file.Body, 0o755); err != nil {
				return fmt.Errorf("install CoreDNS: %w", err)
			}

			found = true

			break
		}

		if !found {
			return fmt.Errorf("CoreDNS archive does not contain coredns")
		}
	} else if err := source.DownloadWithSHA256Verification(ctx, expectedHash, destination, 0o755); err != nil {
		return fmt.Errorf("download CoreDNS binary: %w", err)
	}

	// CoreDNS was written moments ago, so this exec can transiently fail with
	// ETXTBSY if anything else in the process forked while it was being
	// written. ConfigureLocalDNS runs under phases.Parallel alongside five
	// other tasks that fork constantly, so that is not hypothetical.
	var output string

	err = executil.RetryWhileTextFileBusy(ctx, c.log, func() error {
		raw, runErr := exec.CommandContext(ctx, destination, "-plugins").CombinedOutput()
		output = string(raw)

		return runErr
	})
	if err != nil {
		return fmt.Errorf("list CoreDNS plugins: %w", err)
	}

	plugins := map[string]struct{}{}
	for _, field := range strings.Fields(output) {
		plugins[strings.TrimPrefix(field, "dns.")] = struct{}{}
	}

	for _, required := range c.goalState.LocalDNS.RequiredPlugins {
		if _, ok := plugins[required]; !ok {
			return fmt.Errorf("CoreDNS binary is missing required plugin %q", required)
		}
	}

	c.log.Info("installed CoreDNS", "version", c.goalState.LocalDNS.CoreDNSVersion)

	return nil
}

func localDNSResolvConf(original []byte, listener string) []byte {
	var lines []string

	for _, line := range strings.Split(string(original), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "nameserver" {
			continue
		}

		if line != "" {
			lines = append(lines, line)
		}
	}

	lines = append(lines, "nameserver "+listener)

	return []byte(strings.Join(lines, "\n") + "\n")
}

func ensureWorldExecutableDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}

	return os.Chmod(path, 0o755)
}

func coreDNSDownloadSource(rootFS *goalstates.RootFS) *goalstates.DownloadSource {
	if rootFS.Downloads == nil {
		return nil
	}

	return rootFS.Downloads.CoreDNS
}

func enableMachineUnit(machineDir, unit string) error {
	wants := filepath.Join(machineDir, "etc/systemd/system/multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		return fmt.Errorf("create systemd wants directory: %w", err)
	}

	link := filepath.Join(wants, unit)

	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace %s enablement link: %w", unit, err)
	}

	if err := os.Symlink("../"+unit, link); err != nil {
		return fmt.Errorf("enable %s: %w", unit, err)
	}

	return nil
}
