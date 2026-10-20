// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nvidia

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const healthyGPU = "GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-12345678-1234-1234-1234-123456789abc)"

func TestCheckReadiness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		output  string
		err     error
		missing bool
		wantErr string
	}{
		{name: "healthy GPU", output: healthyGPU},
		{name: "multiple GPUs", output: healthyGPU + "\nGPU 1: Tesla T4 (UUID: GPU-abcdef12-1234-1234-1234-123456789abc)\n"},
		{name: "MIG enabled", output: healthyGPU + "\n  MIG 1g.10gb Device 0: (UUID: MIG-abcdef12-1234-1234-1234-123456789abc)\n"},
		{name: "aligned MIG columns", output: healthyGPU + "\n  MIG 1g.10gb     Device  0: (UUID: MIG-abcdef12-1234-1234-1234-123456789abc)\n"},
		{name: "legacy MIG UUID", output: healthyGPU + "\n  MIG 1g.10gb Device 0: (UUID: MIG-GPU-abcdef12-1234-1234-1234-123456789abc/1/0)\n"},
		{name: "no devices exit zero", output: "No devices found.\n", wantErr: "valid NVIDIA GPU enumeration"},
		{name: "no devices alternate", output: "No devices were found\n", wantErr: "valid NVIDIA GPU enumeration"},
		{name: "empty output", wantErr: "valid NVIDIA GPU enumeration"},
		{name: "whitespace output", output: " \n\t ", wantErr: "valid NVIDIA GPU enumeration"},
		{name: "unexpected output", output: "driver initialized", wantErr: "valid NVIDIA GPU enumeration"},
		{name: "malformed GPU", output: "GPU 0: Tesla T4", wantErr: "valid NVIDIA GPU enumeration"},
		{name: "MIG without GPU", output: "MIG 1g.10gb Device 0: (UUID: MIG-1234)", wantErr: "valid NVIDIA GPU enumeration"},
		{name: "mixed error output", output: healthyGPU + "\nNo devices found.", wantErr: "valid NVIDIA GPU enumeration"},
		{name: "command failure", err: errors.New("exit status 1"), wantErr: "GPU query failed"},
		{name: "failure with GPU output", output: healthyGPU, err: errors.New("exit status 1"), wantErr: "GPU query failed"},
		{name: "missing tooling", missing: true, wantErr: "nvidia-smi is not installed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := CheckReadiness(context.Background(), func(name string) (string, error) {
				require.Equal(t, "nvidia-smi", name)

				if tt.missing {
					return "", errors.New("not found")
				}

				return "/usr/bin/nvidia-smi", nil
			}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
				require.Equal(t, "/usr/bin/nvidia-smi", name)
				require.Equal(t, []string{"-L"}, args)

				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.Positive(t, time.Until(deadline))
				require.LessOrEqual(t, time.Until(deadline), 20*time.Second)

				return []byte(tt.output), tt.err
			})
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
				require.ErrorContains(t, err, "repair the host NVIDIA driver")
			}
		})
	}
}

func TestCheckReadinessTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := CheckReadiness(ctx, func(string) (string, error) {
		return "nvidia-smi", nil
	}, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()

		// Even successful-looking output cannot override a timed-out query.
		return []byte(healthyGPU), nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "GPU query interrupted")
}
