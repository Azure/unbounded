// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	agentUpgradeDownloadURLParameter = "downloadURL"
	agentUpgradeSHA256Parameter      = "sha256"
	agentUpgradeBinaryMode           = 0o755
)

type agentUpgradeRequest struct {
	downloadURL string
	sha256      string
}

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

func parseAgentUpgradeRequest(parameters map[string]string) (agentUpgradeRequest, error) {
	request := agentUpgradeRequest{
		downloadURL: strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter]),
		sha256:      strings.TrimSpace(parameters[agentUpgradeSHA256Parameter]),
	}
	if request.downloadURL == "" {
		return agentUpgradeRequest{}, fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}

	return request, nil
}

func upgradeDaemonBinary(ctx context.Context, log *slog.Logger, request agentUpgradeRequest) error {
	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}

	layout := agentbinary.Layout{
		BinaryPath:   paths.BinaryPath,
		BluePath:     paths.BluePath,
		GreenPath:    paths.GreenPath,
		CurrentPath:  paths.CurrentPath,
		LastGoodPath: paths.LastGoodPath,
	}
	_, err = agentbinary.InstallAndSwitchFromTarGz(ctx, log, layout, agentbinary.InstallOptions{
		DownloadURL:    request.downloadURL,
		ExpectedSHA256: request.sha256,
		ExpectedMember: goalstates.AgentUpgradeBinaryName,
		Mode:           agentUpgradeBinaryMode,
		ExactMember:    true,
	})

	return err
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
		OperationName:             pending.OperationName,
		ObservedMachineGeneration: pending.ObservedMachineGeneration,
		FailureMessage:            message,
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
