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
	// FailureMessage is set only after recovery reports a failed upgraded daemon.
	FailureMessage string `json:"failureMessage,omitempty"`
}

// agentUpgradeSignalOperator manages persistent AgentUpgrade signal files.
type agentUpgradeSignalOperator interface {
	RecordPending(operationName string, observedMachineGeneration int64) error
	RecordFailure(message string) error
	Read() (*agentUpgradeSignal, error)
	Clear() error
}

// fileAgentUpgradeSignalOperator stores AgentUpgrade signals on disk.
type fileAgentUpgradeSignalOperator struct {
	path string
}

func agentUpgradeDownloadURL(parameters map[string]string) (string, error) {
	downloadURL := strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter])
	if downloadURL == "" {
		return "", fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}

	return downloadURL, nil
}

func upgradeDaemonBinary(ctx context.Context, log *slog.Logger, downloadURL string) error {
	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}
	targetPath := paths.NextTargetPath()
	if err := agentbinary.InstallAndSwitchFromTarGz(ctx, downloadURL, paths, agentUpgradeBinaryMode); err != nil {
		return err
	}

	log.Info("staged upgraded daemon binary",
		"url", downloadURL,
		"previous", paths.CurrentTargetPath,
		"current", targetPath,
	)

	return nil
}

func newAgentUpgradeSignalOperator() (agentUpgradeSignalOperator, error) {
	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return nil, fmt.Errorf("resolve AgentUpgrade signal path: %w", err)
	}

	return newAgentUpgradeSignalOperatorForPath(paths.SignalPath), nil
}

func newAgentUpgradeSignalOperatorForPath(path string) agentUpgradeSignalOperator {
	return fileAgentUpgradeSignalOperator{path: path}
}

func (o fileAgentUpgradeSignalOperator) RecordPending(operationName string, observedMachineGeneration int64) error {
	return o.write(agentUpgradeSignal{
		OperationName:             operationName,
		ObservedMachineGeneration: observedMachineGeneration,
	})
}

func (o fileAgentUpgradeSignalOperator) RecordFailure(message string) error {
	pending, err := o.Read()
	if err != nil {
		return fmt.Errorf("read pending AgentUpgrade operation signal: %w", err)
	}
	if pending == nil {
		slog.Warn("no pending AgentUpgrade operation signal found; skipping failure signal", "path", o.path)
		return nil
	}

	message = strings.TrimSpace(message)
	if message == "" {
		message = "AgentUpgrade daemon failed after switching binary"
	}

	return o.write(agentUpgradeSignal{
		OperationName:  pending.OperationName,
		FailureMessage: message,
	})
}

func (o fileAgentUpgradeSignalOperator) Clear() error {
	if err := os.Remove(o.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func (o fileAgentUpgradeSignalOperator) write(signal agentUpgradeSignal) error {
	data, err := json.Marshal(signal)
	if err != nil {
		return err
	}

	return writeFile(o.path, append(data, '\n'), 0o600)
}

func (o fileAgentUpgradeSignalOperator) Read() (*agentUpgradeSignal, error) {
	data, err := os.ReadFile(o.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}

	var signal agentUpgradeSignal
	if err := json.Unmarshal(data, &signal); err != nil {
		return nil, fmt.Errorf("decode AgentUpgrade signal %s: %w", o.path, err)
	}
	signal.OperationName = strings.TrimSpace(signal.OperationName)
	signal.FailureMessage = strings.TrimSpace(signal.FailureMessage)
	if signal.OperationName == "" {
		slog.Warn("AgentUpgrade signal missing operation name; ignoring signal", "path", o.path)
		return nil, nil
	}

	return &signal, nil
}

// RecordAgentUpgradeFailureSignal records that the daemon failed after an
// AgentUpgrade.
func RecordAgentUpgradeFailureSignal(message string) error {
	signals, err := newAgentUpgradeSignalOperator()
	if err != nil {
		return err
	}

	return signals.RecordFailure(message)
}

func ensureDaemonBinaryLinks(log *slog.Logger) error {
	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}

	currentTarget := paths.CurrentTargetPath
	if _, err := os.Lstat(paths.CurrentPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat current daemon binary symlink: %w", err)
		}
		target, targetErr := initialDaemonBinaryTarget(paths)
		if targetErr != nil {
			return fmt.Errorf("no executable agent binary found for daemon link initialization: %w", targetErr)
		}
		if err := agentbinary.UpdateSymlink(paths.CurrentPath, target); err != nil {
			return fmt.Errorf("initialize current daemon symlink: %w", err)
		}
		currentTarget = target
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

func initialDaemonBinaryTarget(paths goalstates.AgentUpgradePaths) (string, error) {
	target, err := paths.InitialDaemonBinaryTarget()
	if err != nil {
		return "", err
	}
	if target != paths.BinaryPath {
		return target, nil
	}
	if err := agentbinary.InstallFromFile(paths.BinaryPath, paths.BluePath, agentUpgradeBinaryMode); err != nil {
		return "", err
	}

	return paths.BluePath, nil
}
