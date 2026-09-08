// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
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
	defaultCNIInspectionRoot          = "/proc"
	cniBlockReminderInterval          = 5 * time.Minute
	cniDiagnosticMaxFindings          = 3
	cniDiagnosticMaxFindingCharacters = 384
)

type bridgePodCIDRInspector func(ctx context.Context, bridgeName, procRoot string, cidrs []string) error

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

	procRoot := cfg.cniProcRoot
	if procRoot == "" {
		procRoot = defaultCNIInspectionRoot
	}

	return inspector(ctx, cfg.BridgeName, procRoot, podCIDRs)
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

		disabledPath, disableErr := disableOwnedCNIConfig(cfg)

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
	if err := removeOwnedDisabledCNIConfig(cfg, disabledPath); err != nil {
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

func disableOwnedCNIConfig(cfg *config) (string, error) {
	activePath := cniConfigPath(cfg)
	disabledPath := cniDisabledPath(cfg)

	activeOwned, activeExists, err := inspectOwnedCNIConfig(activePath, cfg.BridgeName)
	if err != nil {
		return "", fmt.Errorf("inspect active CNI configuration %s: %w", activePath, err)
	}

	if !activeExists {
		disabledOwned, disabledExists, disabledErr := inspectOwnedCNIConfig(disabledPath, cfg.BridgeName)
		if disabledErr != nil {
			return "", fmt.Errorf("inspect disabled CNI configuration %s: %w", disabledPath, disabledErr)
		}

		if disabledExists && !disabledOwned {
			return "", fmt.Errorf("foreign or malformed disabled CNI configuration exists at %s", disabledPath)
		}

		if disabledExists {
			return disabledPath, nil
		}

		return "", nil
	}

	if !activeOwned {
		return "", fmt.Errorf("refusing to disable foreign or malformed CNI configuration at %s", activePath)
	}

	_, disabledExists, err := inspectOwnedCNIConfig(disabledPath, cfg.BridgeName)
	if err != nil {
		return "", fmt.Errorf("inspect disabled CNI configuration %s: %w", disabledPath, err)
	}

	if disabledExists {
		return "", fmt.Errorf("refusing to overwrite existing disabled CNI configuration at %s", disabledPath)
	}

	if err := cfg.renameFile(activePath, disabledPath); err != nil {
		return "", fmt.Errorf("disable owned CNI configuration %s: %w", activePath, err)
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

	activeOwned, activeExists, err := inspectOwnedCNIConfig(activePath, cfg.BridgeName)
	if err != nil {
		return fmt.Errorf("inspect active CNI configuration %s: %w", activePath, err)
	}

	if activeExists && !activeOwned {
		return fmt.Errorf("refusing to overwrite foreign or malformed CNI configuration at %s", activePath)
	}

	disabledPath := cniDisabledPath(cfg)

	disabledOwned, disabledExists, err := inspectOwnedCNIConfig(disabledPath, cfg.BridgeName)
	if err != nil {
		return fmt.Errorf("inspect disabled CNI configuration %s: %w", disabledPath, err)
	}

	if disabledExists && !disabledOwned {
		return fmt.Errorf("refusing to overwrite or remove foreign or malformed disabled CNI configuration at %s", disabledPath)
	}

	return nil
}

func removeOwnedDisabledCNIConfig(cfg *config, path string) error {
	owned, exists, err := inspectOwnedCNIConfig(path, cfg.BridgeName)
	if err != nil {
		return err
	}

	if !exists {
		return nil
	}

	if !owned {
		return fmt.Errorf("refusing to remove foreign or malformed file")
	}

	if err := cfg.removeFile(path); err != nil {
		return err
	}

	return nil
}

func inspectOwnedCNIConfig(path, bridgeName string) (owned, exists bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}

		return false, true, err
	}

	if !info.Mode().IsRegular() {
		return false, true, fmt.Errorf("refusing non-regular CNI configuration file")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return false, true, err
	}

	var conflist CNIConfig
	if err := json.Unmarshal(data, &conflist); err != nil {
		return false, true, nil
	}

	if conflist.CNIVersion == "" || conflist.Name != "unbounded-net" || len(conflist.Plugins) == 0 {
		return false, true, nil
	}

	bridgePlugin := conflist.Plugins[0]

	return bridgePlugin.Type == "bridge" && bridgePlugin.Bridge == bridgeName &&
		bridgePlugin.IPAM != nil && bridgePlugin.IPAM.Type == "host-local", true, nil
}
