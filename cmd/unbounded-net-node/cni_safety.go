// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

const (
	configPodCIDRGuard                = "configPodCIDRGuard"
	cniBlockReminderInterval          = 5 * time.Minute
	cniDiagnosticMaxFindings          = 3
	cniDiagnosticMaxFindingCharacters = 384
)

type bridgePodCIDRInspector func(ctx context.Context, bridgeName string, cidrs []string) error

type cniGuardError struct {
	reason string
	cause  error
}

func (e *cniGuardError) Error() string { return e.reason }

func (e *cniGuardError) Unwrap() error { return e.cause }

func managedCNIWriteRequired(configurationChanged bool, healthState *nodeHealthState) bool {
	return configurationChanged || (healthState != nil && healthState.cniNeedsWrite())
}

func (cfg *config) inspectBridgePodCIDRs(ctx context.Context, podCIDRs []string) error {
	inspector := cfg.cniInspector
	if inspector == nil {
		inspector = unboundednetnetlink.InspectBridgePodCIDRs
	}

	return inspector(ctx, cfg.BridgeName, podCIDRs)
}

func (cfg *config) renameFile(oldPath, newPath string) error {
	if cfg.cniRename != nil {
		return cfg.cniRename(oldPath, newPath)
	}

	return os.Rename(oldPath, newPath)
}

func (cfg *config) removeFile(path string) error {
	if cfg.cniRemove != nil {
		return cfg.cniRemove(path)
	}

	return os.Remove(path)
}

func (cfg *config) writeFile(path string, data []byte, mode os.FileMode) error {
	if cfg.cniWriteFile != nil {
		return cfg.cniWriteFile(path, data, mode)
	}

	return os.WriteFile(path, data, mode)
}

func guardedWriteCNIConfig(ctx context.Context, cfg *config, podCIDRs []string, healthState *nodeHealthState) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if healthState != nil {
		healthState.beginCNIWrite(cfg.BridgeName, podCIDRs)
	}

	if err := cfg.inspectBridgePodCIDRs(ctx, podCIDRs); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		disabledPath, disableErr := disableManagedCNIConfig(cfg)

		reason := formatCNIGuardReason(cfg, podCIDRs, err, disabledPath, disableErr)
		if healthState != nil {
			healthState.setCNIBlocked(reason)
		}

		return &cniGuardError{reason: reason, cause: errors.Join(err, disableErr)}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := validateCNIConfigPathsForWrite(cfg); err != nil {
		reason := formatCNIGuardReason(cfg, podCIDRs, err, "", nil)
		if healthState != nil {
			healthState.setCNIBlocked(reason)
		}

		return &cniGuardError{reason: reason, cause: err}
	}

	if err := writeCNIConfigUnchecked(cfg, podCIDRs); err != nil {
		reason := formatCNIGuardReason(cfg, podCIDRs, fmt.Errorf("publish CNI configuration: %w", err), "", nil)
		if healthState != nil {
			healthState.setCNIBlocked(reason)
		}

		return &cniGuardError{reason: reason, cause: err}
	}

	disabledPath := cniDisabledPath(cfg)
	if err := removeDisabledCNIConfig(cfg, disabledPath); err != nil {
		reason := formatCNIGuardReason(cfg, podCIDRs, fmt.Errorf("clean up disabled CNI configuration %s: %w", disabledPath, err), disabledPath, nil)
		if healthState != nil {
			healthState.setCNIBlocked(reason)
		}

		return &cniGuardError{reason: reason, cause: err}
	}

	if healthState != nil {
		healthState.setCNIReady(cfg.BridgeName, podCIDRs)
	}

	return nil
}

func formatCNIGuardReason(cfg *config, podCIDRs []string, inspectionErr error, disabledPath string, disableErr error) string {
	parts := []string{
		"CNI configuration blocked; remaining unready",
		fmt.Sprintf("bridge=%s", cfg.BridgeName),
		fmt.Sprintf("assignedPodCIDRs=%v", podCIDRs),
		fmt.Sprintf("reason=%s", summarizeCNIDiagnostic(inspectionErr)),
	}
	if disabledPath != "" {
		parts = append(parts, fmt.Sprintf("disabledPath=%s", disabledPath))
	}

	if disableErr != nil {
		parts = append(parts, fmt.Sprintf("disableError=%v", disableErr))
	}

	return strings.Join(parts, " ")
}

func summarizeCNIDiagnostic(err error) string {
	if err == nil {
		return ""
	}

	type multiUnwrapper interface {
		Unwrap() []error
	}

	var (
		findings []string
		total    int
	)

	var visit func(error)

	visit = func(current error) {
		if current == nil {
			return
		}

		if joined, ok := current.(multiUnwrapper); ok {
			for _, child := range joined.Unwrap() {
				visit(child)
			}

			return
		}

		total++

		if len(findings) < cniDiagnosticMaxFindings {
			findings = append(findings, truncateCNIDiagnostic(current.Error(), cniDiagnosticMaxFindingCharacters))
		}
	}

	visit(err)

	if total == 0 {
		return truncateCNIDiagnostic(err.Error(), cniDiagnosticMaxFindingCharacters)
	}

	summary := strings.Join(findings, "; ")
	if omitted := total - len(findings); omitted > 0 {
		summary = fmt.Sprintf("%s; %d additional finding(s) omitted", summary, omitted)
	}

	return summary
}

func truncateCNIDiagnostic(message string, maxCharacters int) string {
	normalized := strings.Join(strings.Fields(message), " ")

	runes := []rune(normalized)
	if len(runes) <= maxCharacters {
		return normalized
	}

	return string(runes[:maxCharacters]) + "...[truncated]"
}

func cniConfigPath(cfg *config) string {
	return filepath.Join(cfg.CNIConfDir, cfg.CNIConfFile)
}

func cniDisabledPath(cfg *config) string {
	return cniConfigPath(cfg) + ".disabled"
}

func disableManagedCNIConfig(cfg *config) (string, error) {
	activePath := cniConfigPath(cfg)
	disabledPath := cniDisabledPath(cfg)

	activeExists, err := inspectCNIConfigFile(activePath)
	if err != nil {
		return "", fmt.Errorf("inspect active CNI configuration %s: %w", activePath, err)
	}

	disabledExists, err := inspectCNIConfigFile(disabledPath)
	if err != nil {
		return "", fmt.Errorf("inspect disabled CNI configuration %s: %w", disabledPath, err)
	}

	if !activeExists {
		if disabledExists {
			return disabledPath, nil
		}

		return "", nil
	}

	// The disabled path is a replaceable snapshot of the most recently disabled
	// managed file, not an ownership marker or an input to bridge safety.
	if err := cfg.renameFile(activePath, disabledPath); err != nil {
		return "", fmt.Errorf("disable managed CNI configuration %s: %w", activePath, err)
	}

	return disabledPath, nil
}

func validateCNIConfigPathsForWrite(cfg *config) error {
	activePath := cniConfigPath(cfg)

	tmpPath := activePath + ".tmp"
	if _, err := os.Lstat(tmpPath); err == nil {
		return fmt.Errorf("refusing to overwrite existing temporary CNI configuration at %s", tmpPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect temporary CNI configuration %s: %w", tmpPath, err)
	}

	if _, err := inspectCNIConfigFile(activePath); err != nil {
		return fmt.Errorf("inspect active CNI configuration %s: %w", activePath, err)
	}

	disabledPath := cniDisabledPath(cfg)

	if _, err := inspectCNIConfigFile(disabledPath); err != nil {
		return fmt.Errorf("inspect disabled CNI configuration %s: %w", disabledPath, err)
	}

	return nil
}

func removeDisabledCNIConfig(cfg *config, path string) error {
	exists, err := inspectCNIConfigFile(path)
	if err != nil {
		return err
	}

	if !exists {
		return nil
	}

	if err := cfg.removeFile(path); err != nil {
		return err
	}

	return nil
}

func inspectCNIConfigFile(path string) (exists bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return true, err
	}

	if !info.Mode().IsRegular() {
		return true, fmt.Errorf("refusing non-regular CNI configuration file")
	}

	return true, nil
}
