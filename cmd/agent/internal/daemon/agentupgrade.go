// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	agentUpgradeDownloadURLParameter = "downloadURL"
	agentUpgradeBinaryMode           = 0o755
)

// agentUpgradeSignal is the JSON payload for pending and failure signals.
type agentUpgradeSignal struct {
	OperationName             string `json:"operationName"`
	ObservedMachineGeneration int64  `json:"observedMachineGeneration,omitempty"`
	Message                   string `json:"message,omitempty"`
}

// agentUpgradeSignalOperator manages persistent AgentUpgrade signal files.
type agentUpgradeSignalOperator interface {
	RecordPending(operationName string, observedMachineGeneration int64) error
	RecordFailure(message string) error
	ReadPending() (*agentUpgradeSignal, error)
	ReadFailure() (*agentUpgradeSignal, error)
	RemovePending() error
	RemoveFailure() error
	Clear() error
}

// fileAgentUpgradeSignalOperator stores AgentUpgrade signals on disk.
type fileAgentUpgradeSignalOperator struct {
	paths goalstates.AgentUpgradeSignalPaths
}

func agentUpgradeDownloadURL(parameters map[string]string) (string, error) {
	downloadURL := strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter])
	if downloadURL == "" {
		return "", fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}

	return downloadURL, nil
}

func upgradeDaemonBinary(ctx context.Context, log *slog.Logger, downloadURL string) error {
	paths := goalstates.ResolvedAgentUpgradePaths()
	currentTarget, err := resolveSymlink(paths.CurrentPath, paths.BinaryPath)
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}
	upgrade := paths.ResolveAgentUpgrade(downloadURL, currentTarget)

	if err := agentbinary.InstallFromTarGz(ctx, upgrade.DownloadURL, upgrade.TargetBinaryPath, upgrade.BinaryName, agentUpgradeBinaryMode); err != nil {
		return fmt.Errorf("install upgraded daemon binary to %s: %w", upgrade.TargetBinaryPath, err)
	}

	if err := agentbinary.UpdateSymlink(upgrade.LastGoodLinkPath, upgrade.PreviousBinaryPath); err != nil {
		return fmt.Errorf("update last-good daemon symlink: %w", err)
	}

	if err := agentbinary.UpdateSymlink(upgrade.CurrentLinkPath, upgrade.TargetBinaryPath); err != nil {
		return fmt.Errorf("update current daemon symlink: %w", err)
	}

	log.Info("staged upgraded daemon binary",
		"url", upgrade.DownloadURL,
		"previous", upgrade.PreviousBinaryPath,
		"current", upgrade.TargetBinaryPath,
	)

	return nil
}

func newAgentUpgradeSignalOperator() agentUpgradeSignalOperator {
	return fileAgentUpgradeSignalOperator{
		paths: goalstates.ResolvedAgentUpgradeSignalPaths(),
	}
}

func newAgentUpgradeSignalOperatorForPaths(paths goalstates.AgentUpgradeSignalPaths) agentUpgradeSignalOperator {
	return fileAgentUpgradeSignalOperator{paths: paths}
}

func (o fileAgentUpgradeSignalOperator) RecordPending(operationName string, observedMachineGeneration int64) error {
	return o.write(o.paths.OperationPath, agentUpgradeSignal{
		OperationName:             operationName,
		ObservedMachineGeneration: observedMachineGeneration,
	})
}

func (o fileAgentUpgradeSignalOperator) RecordFailure(message string) error {
	pending, err := o.ReadPending()
	if err != nil {
		return fmt.Errorf("read pending AgentUpgrade operation signal: %w", err)
	}
	if pending == nil {
		return nil
	}

	message = strings.TrimSpace(message)
	if message == "" {
		message = "AgentUpgrade daemon failed after switching binary"
	}

	if err := o.write(o.paths.FailurePath, agentUpgradeSignal{
		OperationName: pending.OperationName,
		Message:       message,
	}); err != nil {
		return err
	}

	return o.RemovePending()
}

func (o fileAgentUpgradeSignalOperator) ReadPending() (*agentUpgradeSignal, error) {
	return o.read(o.paths.OperationPath)
}

func (o fileAgentUpgradeSignalOperator) ReadFailure() (*agentUpgradeSignal, error) {
	return o.read(o.paths.FailurePath)
}

func (o fileAgentUpgradeSignalOperator) RemovePending() error {
	return removeAgentUpgradeSignal(o.paths.OperationPath)
}

func (o fileAgentUpgradeSignalOperator) RemoveFailure() error {
	return removeAgentUpgradeSignal(o.paths.FailurePath)
}

func (o fileAgentUpgradeSignalOperator) Clear() error {
	if err := o.RemovePending(); err != nil {
		return err
	}
	return o.RemoveFailure()
}

func (o fileAgentUpgradeSignalOperator) write(path string, signal agentUpgradeSignal) error {
	data, err := json.Marshal(signal)
	if err != nil {
		return err
	}

	return writeFile(path, append(data, '\n'), 0o600)
}

func (o fileAgentUpgradeSignalOperator) read(path string) (*agentUpgradeSignal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}

	var signal agentUpgradeSignal
	if err := json.Unmarshal(data, &signal); err != nil {
		return nil, fmt.Errorf("decode AgentUpgrade signal %s: %w", path, err)
	}
	signal.OperationName = strings.TrimSpace(signal.OperationName)
	signal.Message = strings.TrimSpace(signal.Message)
	if signal.OperationName == "" {
		return nil, nil
	}

	return &signal, nil
}

// RecordAgentUpgradeFailureSignal records that the daemon failed after an
// AgentUpgrade and removes the pending operation signal.
func RecordAgentUpgradeFailureSignal(operationPath, failurePath, message string) error {
	operator := newAgentUpgradeSignalOperatorForPaths(goalstates.AgentUpgradeSignalPaths{
		OperationPath: operationPath,
		FailurePath:   failurePath,
	})
	return operator.RecordFailure(message)
}

func removeAgentUpgradeSignal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func clearAgentUpgradeSignals(log *slog.Logger, operator agentUpgradeSignalOperator) {
	if err := operator.Clear(); err != nil {
		log.Warn("failed to clear AgentUpgrade signals", "error", err)
	}
}

func ensureDaemonBinaryLinks(log *slog.Logger) error {
	paths := goalstates.ResolvedAgentUpgradePaths()

	if _, err := filepath.EvalSymlinks(paths.CurrentPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve current daemon binary symlink: %w", err)
		}
		target, targetErr := paths.InitialDaemonBinaryTarget()
		if targetErr != nil {
			return fmt.Errorf("no executable agent binary found for daemon link initialization: %w", targetErr)
		}
		if err := agentbinary.UpdateSymlink(paths.CurrentPath, target); err != nil {
			return fmt.Errorf("initialize current daemon symlink: %w", err)
		}
	}

	currentTarget, err := filepath.EvalSymlinks(paths.CurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}

	if _, err := filepath.EvalSymlinks(paths.LastGoodPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve last-good daemon binary symlink: %w", err)
		}
		if err := agentbinary.UpdateSymlink(paths.LastGoodPath, currentTarget); err != nil {
			return fmt.Errorf("initialize last-good daemon symlink: %w", err)
		}
	}

	if currentTarget != paths.BinaryPath {
		// Do not replace the compatibility path when the current symlink
		// already resolves to that path. That preserves legacy installs and
		// avoids creating a BinaryPath -> CurrentPath -> BinaryPath loop.
		if err := agentbinary.UpdateSymlink(paths.BinaryPath, paths.CurrentPath); err != nil {
			return fmt.Errorf("initialize daemon compatibility symlink: %w", err)
		}
	}

	log.Info("daemon binary links initialized",
		"current", paths.CurrentPath,
		"last_good", paths.LastGoodPath,
	)

	return nil
}

func resolveSymlink(path, fallbackPath string) (string, error) {
	targetPath, err := filepath.EvalSymlinks(path)
	if err == nil {
		return targetPath, nil
	}

	if os.IsNotExist(err) {
		return fallbackPath, nil
	}

	return "", err
}
