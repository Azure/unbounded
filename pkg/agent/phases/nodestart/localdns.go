// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type configureLocalDNS struct {
	goalState *goalstates.NodeStart
}

// ConfigureLocalDNS refreshes machine-local CoreDNS inputs before each managed start.
func ConfigureLocalDNS(goalState *goalstates.NodeStart) phases.Task {
	return &configureLocalDNS{goalState: goalState}
}

func (c *configureLocalDNS) Name() string { return "configure-localdns" }

func (c *configureLocalDNS) Do(_ context.Context) error {
	if !c.goalState.LocalDNS.Enabled {
		return nil
	}

	root := filepath.Join(c.goalState.MachineDir, "etc/unbounded/localdns")
	if err := utilio.WriteFile(filepath.Join(root, "Corefile"), c.goalState.LocalDNS.Corefile, 0o644); err != nil {
		return fmt.Errorf("write LocalDNS Corefile: %w", err)
	}

	upstreams := make([]string, 0, len(c.goalState.LocalDNS.NodeUpstreamIPs))
	for _, upstream := range c.goalState.LocalDNS.NodeUpstreamIPs {
		upstreams = append(upstreams, upstream.String())
	}

	if err := utilio.WriteFile(filepath.Join(root, "node-upstreams"), []byte(strings.Join(upstreams, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("write LocalDNS upstreams: %w", err)
	}

	environment := fmt.Sprintf("NODE_LISTENER=%s\nCLUSTER_LISTENER=%s\n", c.goalState.LocalDNS.NodeListenerIP, c.goalState.LocalDNS.ClusterListenerIP)
	if err := utilio.WriteFile(filepath.Join(root, "environment"), []byte(environment), 0o644); err != nil {
		return fmt.Errorf("write LocalDNS environment: %w", err)
	}

	var slice bytes.Buffer
	if err := assetsTemplate.ExecuteTemplate(&slice, "localdns.slice", map[string]any{
		"CPUQuota":  float64(c.goalState.LocalDNS.CPULimitInMilliCores) / 10,
		"MemoryMax": c.goalState.LocalDNS.MemoryLimitInMB,
	}); err != nil {
		return fmt.Errorf("render LocalDNS slice: %w", err)
	}

	if err := utilio.WriteFile(filepath.Join(c.goalState.MachineDir, "etc/systemd/system", goalstates.LocalDNSSliceUnit), slice.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write LocalDNS slice: %w", err)
	}

	var resolverLines []string

	for _, line := range strings.Split(string(c.goalState.LocalDNS.OriginalHostResolvConf), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "nameserver" {
			continue
		}

		if line != "" {
			resolverLines = append(resolverLines, line)
		}
	}

	resolverLines = append(resolverLines, "nameserver "+c.goalState.LocalDNS.NodeListenerIP.String())

	resolverContent := []byte(strings.Join(resolverLines, "\n") + "\n")
	for _, resolvPath := range []string{
		filepath.Join(c.goalState.MachineDir, "etc/resolv.conf"),
		filepath.Join(c.goalState.MachineDir, strings.TrimPrefix(goalstates.LocalDNSResolvConfPath, "/")),
	} {
		if err := utilio.WriteFile(resolvPath, resolverContent, 0o644); err != nil {
			return fmt.Errorf("write machine resolver %s: %w", resolvPath, err)
		}
	}

	return nil
}

type setupLocalDNSNetwork struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

// SetupLocalDNSNetwork reconciles the shared dummy interface and NOTRACK rules.
func SetupLocalDNSNetwork(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &setupLocalDNSNetwork{log: log, goalState: goalState}
}

func (s *setupLocalDNSNetwork) Name() string { return "setup-localdns-network" }

func (s *setupLocalDNSNetwork) Do(ctx context.Context) error {
	if !s.goalState.LocalDNS.Enabled {
		return nil
	}

	data := map[string]string{
		"MachineName":       s.goalState.MachineName,
		"NodeListenerIP":    s.goalState.LocalDNS.NodeListenerIP.String(),
		"ClusterListenerIP": s.goalState.LocalDNS.ClusterListenerIP.String(),
	}

	var script bytes.Buffer
	if err := assetsTemplate.ExecuteTemplate(&script, "unbounded-localdns-network.sh", data); err != nil {
		return fmt.Errorf("render LocalDNS network script: %w", err)
	}

	if err := utilio.WriteFile("/usr/local/libexec/unbounded-localdns-network", script.Bytes(), 0o755); err != nil {
		return fmt.Errorf("write LocalDNS network script: %w", err)
	}

	var unit bytes.Buffer
	if err := assetsTemplate.ExecuteTemplate(&unit, "unbounded-localdns-network.service", data); err != nil {
		return fmt.Errorf("render LocalDNS network unit: %w", err)
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)
	if err := utilio.WriteFile(unitPath, unit.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write LocalDNS network unit: %w", err)
	}

	dropIn := []byte("[Unit]\nRequires=" + goalstates.LocalDNSNetworkUnit + "\nAfter=" + goalstates.LocalDNSNetworkUnit + "\n")

	dropInPath := filepath.Join(goalstates.SystemdSystemDir, "systemd-nspawn@"+s.goalState.MachineName+".service.d", "10-localdns.conf")
	if err := utilio.WriteFile(dropInPath, dropIn, 0o644); err != nil {
		return fmt.Errorf("write LocalDNS nspawn ordering: %w", err)
	}

	if err := executil.RunCmd(ctx, s.log, executil.Systemctl(), "daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd for LocalDNS: %w", err)
	}

	if err := executil.RunCmd(ctx, s.log, executil.Systemctl(), "restart", goalstates.LocalDNSNetworkUnit); err != nil {
		return fmt.Errorf("start LocalDNS network unit: %w", err)
	}

	return nil
}

type waitLocalDNS struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

// WaitForLocalDNS waits until both CoreDNS readiness endpoints respond.
func WaitForLocalDNS(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &waitLocalDNS{log: log, goalState: goalState}
}

func (w *waitLocalDNS) Name() string { return "wait-localdns" }

func (w *waitLocalDNS) Do(ctx context.Context) error {
	if !w.goalState.LocalDNS.Enabled {
		return nil
	}

	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	transport := &http.Transport{Proxy: nil}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	addresses := []string{w.goalState.LocalDNS.NodeListenerIP.String(), w.goalState.LocalDNS.ClusterListenerIP.String()}

	for {
		if err := localDNSReady(ctx, client, addresses); err == nil {
			return nil
		} else {
			w.log.Debug("LocalDNS readiness check failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("LocalDNS did not become ready")
		case <-ticker.C:
		}
	}
}

func localDNSReady(ctx context.Context, client *http.Client, addresses []string) error {
	for _, address := range addresses {
		url := fmt.Sprintf("http://%s:%d/ready", address, goalstates.LocalDNSReadinessPort)

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("create readiness request for %s: %w", address, err)
		}

		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("query readiness endpoint for %s: %w", address, err)
		}

		_, copyErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()

		if copyErr != nil {
			return fmt.Errorf("read readiness response for %s: %w", address, copyErr)
		}

		if closeErr != nil {
			return fmt.Errorf("close readiness response for %s: %w", address, closeErr)
		}

		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("readiness endpoint for %s returned %s", address, response.Status)
		}
	}

	return nil
}
