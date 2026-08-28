// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	hostDaemonHealthTimeout  = 30 * time.Second
	hostDaemonStableDuration = 3 * time.Second
	hostDaemonHealthPoll     = 250 * time.Millisecond
)

// HostDaemonActivationService manages the Unbounded systemd units used by a
// host-driven agent binary activation.
type HostDaemonActivationService struct {
	log   *slog.Logger
	paths goalstates.AgentUpgradePaths
}

// NewHostDaemonActivationService returns the Unbounded systemd adapter for
// host-driven agent activation.
func NewHostDaemonActivationService(log *slog.Logger, paths goalstates.AgentUpgradePaths) *HostDaemonActivationService {
	return &HostDaemonActivationService{log: log, paths: paths}
}

// Preflight reports whether the installed daemon assets differ from the
// desired assets. It does not change the host.
func (s *HostDaemonActivationService) Preflight(_ context.Context, currentBinaryPath string) (agentbinary.ServicePlan, error) {
	if _, err := os.Stat(s.paths.SignalPath); err == nil {
		return agentbinary.ServicePlan{}, fmt.Errorf("AgentUpgrade MachineOperation signal exists at %s", s.paths.SignalPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return agentbinary.ServicePlan{}, fmt.Errorf("inspect AgentUpgrade MachineOperation signal: %w", err)
	}

	assets, err := s.desiredAssets(currentBinaryPath)
	if err != nil {
		return agentbinary.ServicePlan{}, err
	}

	for path, desired := range assets {
		actual, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) || readErr == nil && !bytes.Equal(actual, desired.content) {
			return agentbinary.ServicePlan{
				UpdateRequired: true,
				Description:    "install or update Unbounded agent daemon systemd assets",
			}, nil
		}

		if readErr != nil {
			return agentbinary.ServicePlan{}, fmt.Errorf("read daemon asset %s: %w", path, readErr)
		}
	}

	return agentbinary.ServicePlan{Description: "Unbounded agent daemon systemd assets are current"}, nil
}

// Prepare installs the daemon service, recovery service, and recovery script.
// It does not reload systemd or restart the daemon.
func (s *HostDaemonActivationService) Prepare(_ context.Context, currentBinaryPath string) error {
	assets, err := s.desiredAssets(currentBinaryPath)
	if err != nil {
		return err
	}

	for path, asset := range assets {
		if err := writeFile(path, asset.content, asset.mode); err != nil {
			return fmt.Errorf("write daemon asset %s: %w", path, err)
		}
	}

	return nil
}

// Reload reloads systemd configuration.
func (s *HostDaemonActivationService) Reload(ctx context.Context) error {
	if err := executil.RunCmd(ctx, s.log, executil.Systemctl(), "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	return nil
}

// Restart restarts the Unbounded daemon synchronously.
func (s *HostDaemonActivationService) Restart(ctx context.Context) error {
	if err := executil.RunCmd(ctx, s.log, executil.Systemctl(), "restart", goalstates.DaemonUnit); err != nil {
		return fmt.Errorf("systemctl restart %s: %w", goalstates.DaemonUnit, err)
	}

	return nil
}

// WaitHealthy waits until systemd reports a stable daemon process executing
// the expected binary target.
func (s *HostDaemonActivationService) WaitHealthy(ctx context.Context, expectedBinaryPath string) error {
	healthCtx, cancel := context.WithTimeout(ctx, hostDaemonHealthTimeout)
	defer cancel()

	expected, err := filepath.EvalSymlinks(expectedBinaryPath)
	if err != nil {
		return fmt.Errorf("resolve expected daemon binary: %w", err)
	}

	var healthySince time.Time

	ticker := time.NewTicker(hostDaemonHealthPoll)
	defer ticker.Stop()

	for {
		healthy, checkErr := s.isExpectedDaemonActive(healthCtx, expected)
		if checkErr == nil && healthy {
			if healthySince.IsZero() {
				healthySince = time.Now()
			} else if time.Since(healthySince) >= hostDaemonStableDuration {
				return nil
			}
		} else {
			healthySince = time.Time{}
		}

		select {
		case <-healthCtx.Done():
			if checkErr != nil {
				return fmt.Errorf("daemon did not become healthy: %w", checkErr)
			}

			return fmt.Errorf("daemon did not execute expected binary %s: %w", expected, healthCtx.Err())
		case <-ticker.C:
		}
	}
}

func (s *HostDaemonActivationService) isExpectedDaemonActive(ctx context.Context, expected string) (bool, error) {
	output, err := executil.OutputCmd(ctx, s.log, "systemctl", "show", "--property", "MainPID", "--value", goalstates.DaemonUnit)
	if err != nil {
		return false, err
	}

	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("invalid daemon MainPID %q", output)
	}

	running, err := filepath.EvalSymlinks(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false, err
	}

	return running == expected, nil
}

type daemonAsset struct {
	content []byte
	mode    os.FileMode
}

func (s *HostDaemonActivationService) desiredAssets(currentBinaryPath string) (map[string]daemonAsset, error) {
	paths := s.paths
	paths.CurrentPath = currentBinaryPath

	service, err := renderDaemonAssetForPaths("daemon-service", daemonServiceContent, paths)
	if err != nil {
		return nil, err
	}

	recoveryService, err := renderDaemonAssetForPaths("daemon-recovery-service", daemonRecoveryServiceContent, paths)
	if err != nil {
		return nil, err
	}

	recoveryScript, err := renderDaemonAssetForPaths("daemon-recovery-script", daemonRecoveryScriptContent, paths)
	if err != nil {
		return nil, err
	}

	return map[string]daemonAsset{
		filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit):         {content: service, mode: 0o644},
		filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit): {content: recoveryService, mode: 0o644},
		paths.RecoveryScriptPath: {content: recoveryScript, mode: 0o755},
	}, nil
}

var _ agentbinary.DaemonService = (*HostDaemonActivationService)(nil)
