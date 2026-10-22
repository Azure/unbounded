// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package nvidia validates host GPU readiness before worker provisioning.
package nvidia

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	gpuLine = regexp.MustCompile(`^GPU [0-9]+: .+ \(UUID: GPU-[[:xdigit:]-]+\)$`)
	migLine = regexp.MustCompile(`^MIG .+[[:space:]]+Device[[:space:]]+[0-9]+: \(UUID: MIG-[[:alnum:]/-]+\)$`)
)

// CheckReadiness requires successful enumeration of at least one physical GPU.
// Command dependencies are shared with the caller's host discovery machinery.
func CheckReadiness(
	ctx context.Context,
	lookPath func(string) (string, error),
	outputCmd func(context.Context, string, ...string) ([]byte, error),
) error {
	err := checkReadiness(ctx, lookPath, outputCmd)
	if err != nil {
		return fmt.Errorf("NVIDIA GPUs are not ready: %w; repair the host NVIDIA driver and verify nvidia-smi -L before retrying bootstrap", err)
	}

	return nil
}

func checkReadiness(
	ctx context.Context,
	lookPath func(string) (string, error),
	outputCmd func(context.Context, string, ...string) ([]byte, error),
) error {
	binary, err := lookPath("nvidia-smi")
	if err != nil {
		return errors.New("nvidia-smi is not installed")
	}

	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	out, err := outputCmd(checkCtx, binary, "-L")
	if checkCtx.Err() != nil {
		return fmt.Errorf("nvidia-smi GPU query interrupted: %w", checkCtx.Err())
	}

	if err != nil {
		if message := strings.TrimSpace(string(out)); message != "" {
			return fmt.Errorf("nvidia-smi GPU query failed: %w: %s", err, message)
		}

		return fmt.Errorf("nvidia-smi GPU query failed: %w", err)
	}

	hasGPU := false

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case gpuLine.MatchString(line):
			hasGPU = true
		case hasGPU && migLine.MatchString(line):
		default:
			return errors.New("nvidia-smi did not report valid NVIDIA GPU enumeration")
		}
	}

	if !hasGPU {
		return errors.New("nvidia-smi did not report any NVIDIA GPUs")
	}

	return nil
}
