// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const testAgentUpgradeSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestBuildMachineOperationWithMachineRef(t *testing.T) {
	t.Parallel()

	opts := &machineOperationCreateOptions{
		name:       "reboot-worker-01",
		kind:       v1alpha3.OperationHostHardReboot,
		machine:    "worker-01",
		ttlSeconds: 3600,
		output:     operationOutputName,
		dryRun:     dryRunNone,
	}

	require.NoError(t, opts.validate())

	op, err := opts.build()
	require.NoError(t, err)
	require.Equal(t, "reboot-worker-01", op.Name)
	require.Equal(t, "worker-01", op.Spec.MachineRef)
	require.Nil(t, op.Spec.MachineSelector)
	require.Equal(t, v1alpha3.OperationHostHardReboot, op.Spec.OperationKind)
	require.NotNil(t, op.Spec.TTLSecondsAfterFinished)
	require.Equal(t, int32(3600), *op.Spec.TTLSecondsAfterFinished)
}

func TestBuildMachineOperationWithSelector(t *testing.T) {
	t.Parallel()

	opts := &machineOperationCreateOptions{
		name:     "reboot-gpu",
		kind:     v1alpha3.OperationNodeReboot,
		selector: "role=gpu,zone in (east,west)",
		output:   operationOutputName,
		dryRun:   dryRunNone,
	}

	require.NoError(t, opts.validate())

	op, err := opts.build()
	require.NoError(t, err)
	require.Empty(t, op.Spec.MachineRef)
	require.NotNil(t, op.Spec.MachineSelector)
	require.Equal(t, "gpu", op.Spec.MachineSelector.MatchLabels["role"])
	require.Len(t, op.Spec.MachineSelector.MatchExpressions, 1)
	require.Equal(t, "zone", op.Spec.MachineSelector.MatchExpressions[0].Key)
	require.Nil(t, op.Spec.Rollout)
}

func TestBuildMachineOperationWithRollout(t *testing.T) {
	t.Parallel()

	opts := &machineOperationCreateOptions{
		name:          "reboot-rack-01",
		kind:          v1alpha3.OperationHostHardReboot,
		selector:      "topology.unbounded-cloud.io/rack=dc-a.rack-01",
		maxConcurrent: "3",
		onFailure:     "Continue",
		output:        operationOutputName,
		dryRun:        dryRunNone,
	}

	require.NoError(t, opts.validate())

	op, err := opts.build()
	require.NoError(t, err)
	require.NotNil(t, op.Spec.Rollout)
	require.NotNil(t, op.Spec.Rollout.MaxConcurrent)
	require.Equal(t, intstr.Int, op.Spec.Rollout.MaxConcurrent.Type)
	require.Equal(t, 3, op.Spec.Rollout.MaxConcurrent.IntValue())
	require.Equal(t, v1alpha3.RolloutFailurePolicyContinue, op.Spec.Rollout.OnFailure)
}

func TestBuildMachineOperationWithPercentageRollout(t *testing.T) {
	t.Parallel()

	opts := &machineOperationCreateOptions{
		name:          "reboot-rack-01",
		kind:          v1alpha3.OperationHostHardReboot,
		selector:      "topology.unbounded-cloud.io/rack=dc-a.rack-01",
		maxConcurrent: "100%",
		output:        operationOutputName,
		dryRun:        dryRunNone,
	}

	require.NoError(t, opts.validate())

	op, err := opts.build()
	require.NoError(t, err)
	require.NotNil(t, op.Spec.Rollout)
	require.NotNil(t, op.Spec.Rollout.MaxConcurrent)
	require.Equal(t, intstr.String, op.Spec.Rollout.MaxConcurrent.Type)
	require.Equal(t, "100%", op.Spec.Rollout.MaxConcurrent.StrVal)
	require.Empty(t, op.Spec.Rollout.OnFailure)
}

func TestBuildMachineOperationParameters(t *testing.T) {
	t.Parallel()

	opts := &machineOperationCreateOptions{
		name:          "upgrade-worker-01",
		kind:          v1alpha3.OperationAgentUpgrade,
		machine:       "worker-01",
		parameterArgs: []string{"downloadURL=https://example.com/agent.tar.gz", "sha256=" + testAgentUpgradeSHA256},
		output:        operationOutputName,
		dryRun:        dryRunNone,
	}

	require.NoError(t, opts.validate())

	op, err := opts.build()
	require.NoError(t, err)
	require.Equal(t, "https://example.com/agent.tar.gz", op.Spec.Parameters["downloadURL"])
	require.Equal(t, testAgentUpgradeSHA256, op.Spec.Parameters["sha256"])
}

func TestValidateMachineOperationRequiresTarget(t *testing.T) {
	t.Parallel()

	opts := &machineOperationCreateOptions{
		name:   "missing-target",
		kind:   v1alpha3.OperationNodeReboot,
		output: operationOutputName,
		dryRun: dryRunNone,
	}

	err := opts.validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "exactly one of --machine or --selector")
}

func TestValidateRolloutFlagsRequireSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		maxConcurrent  string
		onFailure      string
		retainChildren bool
	}{
		{name: "max-concurrent with machine", maxConcurrent: "3"},
		{name: "on-failure with machine", onFailure: "Continue"},
		{name: "both with machine", maxConcurrent: "3", onFailure: "Continue"},
		{name: "retain-children with machine", retainChildren: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := &machineOperationCreateOptions{
				name:           "reboot-worker-01",
				kind:           v1alpha3.OperationHostHardReboot,
				machine:        "worker-01",
				maxConcurrent:  tt.maxConcurrent,
				onFailure:      tt.onFailure,
				retainChildren: tt.retainChildren,
				output:         operationOutputName,
				dryRun:         dryRunNone,
			}

			err := opts.validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), "only allowed with --selector, not with --machine")

			if tt.maxConcurrent != "" {
				require.Contains(t, err.Error(), "--max-concurrent")
			}

			if tt.onFailure != "" {
				require.Contains(t, err.Error(), "--on-failure")
			}

			if tt.retainChildren {
				require.Contains(t, err.Error(), "--retain-children")
			}
		})
	}
}

func TestValidateMaxConcurrent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  bool
	}{
		{value: "1", want: true},
		{value: "42", want: true},
		{value: "1000", want: true},
		{value: "1%", want: true},
		{value: "25%", want: true},
		{value: "100%", want: true},
		{value: "0"},
		{value: "-1"},
		{value: "abc"},
		{value: "1.5"},
		{value: "0%"},
		{value: "05%"},
		{value: "101%"},
		{value: "1001%"},
		{value: "%"},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()

			opts := &machineOperationCreateOptions{
				name:          "reboot-rack-01",
				kind:          v1alpha3.OperationHostHardReboot,
				selector:      "topology.unbounded-cloud.io/rack=dc-a.rack-01",
				maxConcurrent: tt.value,
				output:        operationOutputName,
				dryRun:        dryRunNone,
			}

			err := opts.validate()
			if tt.want {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), "must be an integer of at least 1 or a percentage from 1% to 100%")
		})
	}
}

func TestValidateOnFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  bool
	}{
		{value: "Stop", want: true},
		{value: "Continue", want: true},
		{value: "Pause"},
		{value: "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()

			opts := &machineOperationCreateOptions{
				name:      "reboot-rack-01",
				kind:      v1alpha3.OperationHostHardReboot,
				selector:  "topology.unbounded-cloud.io/rack=dc-a.rack-01",
				onFailure: tt.value,
				output:    operationOutputName,
				dryRun:    dryRunNone,
			}

			err := opts.validate()
			if tt.want {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), "must be Stop or Continue")
		})
	}
}

func TestValidateAgentUpgradeRequiresDownloadURL(t *testing.T) {
	t.Parallel()

	opts := &machineOperationCreateOptions{
		name:    "upgrade-worker-01",
		kind:    v1alpha3.OperationAgentUpgrade,
		machine: "worker-01",
		output:  operationOutputName,
		dryRun:  dryRunNone,
	}

	err := opts.validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "downloadURL")
}

func TestValidateAgentUpgradeRequiresSHA256(t *testing.T) {
	t.Parallel()

	opts := &machineOperationCreateOptions{
		name:          "upgrade-worker-01",
		kind:          v1alpha3.OperationAgentUpgrade,
		machine:       "worker-01",
		parameterArgs: []string{"downloadURL=http://example.com/agent.tar.gz"},
		output:        operationOutputName,
		dryRun:        dryRunNone,
	}

	// SHA-256 is optional when the operation source is trusted.
	require.NoError(t, opts.validate())
}

func TestValidateWaitRejectsStructuredOutput(t *testing.T) {
	t.Parallel()

	for _, output := range []string{operationOutputYAML, operationOutputJSON} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()

			opts := &machineOperationCreateOptions{
				name:    "reboot-worker-01",
				kind:    v1alpha3.OperationHostHardReboot,
				machine: "worker-01",
				wait:    true,
				output:  output,
				dryRun:  dryRunNone,
			}

			err := opts.validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), "--wait cannot be used")
		})
	}
}

func TestMachineOperationDryRunClientYAML(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	opts := &machineOperationCreateOptions{
		name:       "poweroff-worker-01",
		kind:       v1alpha3.OperationHostPowerOff,
		machine:    "worker-01",
		ttlSeconds: -1,
		output:     operationOutputYAML,
		dryRun:     dryRunClient,
		out:        &out,
	}

	require.NoError(t, opts.run(context.Background()))
	require.Contains(t, out.String(), "kind: MachineOperation")
	require.Contains(t, out.String(), "operationKind: HostPowerOff")
	require.Contains(t, out.String(), "machineRef: worker-01")
	require.NotContains(t, out.String(), "ttlSecondsAfterFinished")
}

func TestMachineOperationCreateCommandDryRunYAML(t *testing.T) {
	t.Parallel()

	out, err := executeMachineOperationCreateCommand(
		t,
		"upgrade-worker-01",
		"--kind", string(v1alpha3.OperationAgentUpgrade),
		"--machine", "worker-01",
		"--param", "downloadURL=https://example.com/agent.tar.gz",
		"--param", "sha256="+testAgentUpgradeSHA256,
		"--ttl", "900",
		"--dry-run=client",
		"-o", "yaml",
	)

	require.NoError(t, err)
	require.Contains(t, out, "kind: MachineOperation")
	require.Contains(t, out, "operationKind: AgentUpgrade")
	require.Contains(t, out, "machineRef: worker-01")
	require.Contains(t, out, "downloadURL: https://example.com/agent.tar.gz")
	require.Contains(t, out, "sha256: "+testAgentUpgradeSHA256)
	require.Contains(t, out, "ttlSecondsAfterFinished: 900")
}

func TestMachineOperationCreateCommandDryRunRolloutYAML(t *testing.T) {
	t.Parallel()

	out, err := executeMachineOperationCreateCommand(
		t,
		"reboot-rack-01",
		"--kind", string(v1alpha3.OperationHostHardReboot),
		"--selector", "topology.unbounded-cloud.io/rack=dc-a.rack-01",
		"--max-concurrent", "100%",
		"--on-failure", "Continue",
		"--dry-run=client",
		"-o", "yaml",
	)

	require.NoError(t, err)
	require.Contains(t, out, "machineSelector:")
	require.Contains(t, out, "topology.unbounded-cloud.io/rack: dc-a.rack-01")
	require.Contains(t, out, "rollout:")
	require.Contains(t, out, "maxConcurrent: 100%")
	require.Contains(t, out, "onFailure: Continue")
	require.NotContains(t, out, "childRetention")
}

func TestMachineOperationCreateCommandRetainChildren(t *testing.T) {
	t.Parallel()

	out, err := executeMachineOperationCreateCommand(
		t,
		"reboot-rack-01",
		"--kind", string(v1alpha3.OperationHostHardReboot),
		"--selector", "topology.unbounded-cloud.io/rack=dc-a.rack-01",
		"--retain-children",
		"--dry-run=client",
		"-o", "yaml",
	)

	require.NoError(t, err)
	require.Contains(t, out, "rollout:")
	require.Contains(t, out, "childRetention: All")
	require.NotContains(t, out, "maxConcurrent")
}

func TestMachineOperationCreateCommandDryRunNumericRolloutYAML(t *testing.T) {
	t.Parallel()

	out, err := executeMachineOperationCreateCommand(
		t,
		"upgrade-rack-01",
		"--kind", string(v1alpha3.OperationAgentUpgrade),
		"--selector", "topology.unbounded-cloud.io/rack=dc-a.rack-01",
		"--param", "downloadURL=https://example.com/unbounded-agent-linux-amd64.tar.gz",
		"--max-concurrent", "3",
		"--dry-run=client",
		"-o", "yaml",
	)

	require.NoError(t, err)
	require.Contains(t, out, "rollout:")
	require.Contains(t, out, "maxConcurrent: 3")
	require.NotContains(t, out, "onFailure")
}

func TestMachineOperationCreateCommandRejectsRolloutWithMachine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "max-concurrent", args: []string{"--max-concurrent", "3"}},
		{name: "on-failure", args: []string{"--on-failure", "Continue"}},
		{name: "both", args: []string{"--max-concurrent", "3", "--on-failure", "Continue"}},
		{name: "retain-children", args: []string{"--retain-children"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			args := append(
				[]string{"reboot-worker-01", "--kind", string(v1alpha3.OperationHostHardReboot), "--machine", "worker-01", "--dry-run=client"},
				tt.args...,
			)

			out, err := executeMachineOperationCreateCommand(t, args...)

			require.Error(t, err)
			require.Contains(t, err.Error(), "only allowed with --selector, not with --machine")
			require.Empty(t, out)
		})
	}
}

func TestMachineOperationCreateCommandRejectsInvalidRolloutValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "max-concurrent zero", args: []string{"--max-concurrent", "0"}},
		{name: "max-concurrent text", args: []string{"--max-concurrent", "abc"}},
		{name: "max-concurrent too large for the field", args: []string{"--max-concurrent", "2147483648"}},
		{name: "max-concurrent over 100%", args: []string{"--max-concurrent", "101%"}},
		{name: "on-failure unknown", args: []string{"--on-failure", "Pause"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			args := append(
				[]string{"reboot-rack-01", "--kind", string(v1alpha3.OperationHostHardReboot), "--selector", "topology.unbounded-cloud.io/rack=dc-a.rack-01", "--dry-run=client"},
				tt.args...,
			)

			out, err := executeMachineOperationCreateCommand(t, args...)

			require.Error(t, err)
			require.NotContains(t, err.Error(), "only allowed with --selector")
			require.Empty(t, out)
		})
	}
}

func TestMachineOperationCreateCommandRejectsNegativeTTLFlag(t *testing.T) {
	t.Parallel()

	out, err := executeMachineOperationCreateCommand(
		t,
		"reboot-worker-01",
		"--kind", string(v1alpha3.OperationHostHardReboot),
		"--machine", "worker-01",
		"--ttl=-1",
		"--dry-run=client",
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "--ttl must be 0 or greater")
	require.Empty(t, out)
}

func TestMachineOperationCreateRejectsRemovedKinds(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"HostOutOfBandReboot", "HostOOBReboot", "HostOSReboot"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			require.False(t, isSupportedOperationKind(v1alpha3.OperationKind(kind)))

			out, err := executeMachineOperationCreateCommand(
				t,
				"reboot-worker-01",
				"--kind", kind,
				"--machine", "worker-01",
				"--dry-run=client",
			)

			require.Error(t, err)
			require.Contains(t, err.Error(), `unsupported operation kind "`+kind+`"`)
			require.Empty(t, out)
		})
	}
}

func TestMachineOperationCreateRejectsAllocationKinds(t *testing.T) {
	t.Parallel()

	for _, kind := range []v1alpha3.OperationKind{v1alpha3.OperationHostAllocate, v1alpha3.OperationHostRelease} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()

			require.False(t, isSupportedOperationKind(kind))

			out, err := executeMachineOperationCreateCommand(
				t,
				"allocate-worker-01",
				"--kind", string(kind),
				"--machine", "worker-01",
				"--dry-run=client",
			)

			require.Error(t, err)
			require.Contains(t, err.Error(), `unsupported operation kind "`+string(kind)+`"`)
			require.Empty(t, out)

			cmd := newMachineOperationCreateCommand(&machineCommandRuntime{})
			require.NotContains(t, cmd.Flags().Lookup("kind").Usage, string(kind))

			complete, ok := cmd.GetFlagCompletionFunc("kind")
			require.True(t, ok)

			completions, _ := complete(cmd, nil, "")
			require.NotEmpty(t, completions)
			require.NotContains(t, completions, string(kind))
		})
	}
}

func TestMachineHostRebootCommandNames(t *testing.T) {
	t.Parallel()

	names := map[string]bool{}

	for _, cmd := range newMachineCommandGroup(&machineCommandRuntime{}).Commands() {
		names[cmd.Name()] = true
		for _, alias := range cmd.Aliases {
			names[alias] = true
		}
	}

	require.True(t, names["host-hard-reboot"])
	require.True(t, names["host-soft-reboot"])

	for _, removed := range []string{"host-out-of-band-reboot", "host-oob-reboot", "host-os-reboot"} {
		require.False(t, names[removed], removed)
	}
}

func TestMachineOperationCreateSmokeCreatesMachineRefOperation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newMachineOperationTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	opts := &machineOperationCreateOptions{
		name:          "upgrade-worker-01",
		kind:          v1alpha3.OperationAgentUpgrade,
		machine:       "worker-01",
		parameterArgs: []string{"downloadURL=https://example.com/agent.tar.gz", "sha256=" + testAgentUpgradeSHA256},
		ttlSeconds:    900,
		output:        operationOutputName,
		dryRun:        dryRunNone,
		fieldManager:  fieldManagerID,
		printCreated:  false,
	}

	require.NoError(t, opts.validate())
	op, err := opts.build()
	require.NoError(t, err)
	require.NoError(t, opts.runWithClient(ctx, c, op))

	var got v1alpha3.MachineOperation
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "upgrade-worker-01"}, &got))
	require.Equal(t, "worker-01", got.Spec.MachineRef)
	require.Nil(t, got.Spec.MachineSelector)
	require.Equal(t, v1alpha3.OperationAgentUpgrade, got.Spec.OperationKind)
	require.Equal(t, "https://example.com/agent.tar.gz", got.Spec.Parameters["downloadURL"])
	require.Equal(t, testAgentUpgradeSHA256, got.Spec.Parameters["sha256"])
	require.NotNil(t, got.Spec.TTLSecondsAfterFinished)
	require.Equal(t, int32(900), *got.Spec.TTLSecondsAfterFinished)
}

func TestMachineOperationCreateSmokeCreatesSelectorOperation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newMachineOperationTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()

	opts := &machineOperationCreateOptions{
		name:         "reboot-gpu-workers",
		kind:         v1alpha3.OperationNodeReboot,
		selector:     "role=gpu,site=edge-1",
		ttlSeconds:   -1,
		output:       operationOutputName,
		dryRun:       dryRunNone,
		fieldManager: fieldManagerID,
		printCreated: false,
	}

	require.NoError(t, opts.validate())
	op, err := opts.build()
	require.NoError(t, err)
	require.NoError(t, opts.runWithClient(ctx, c, op))

	var got v1alpha3.MachineOperation
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "reboot-gpu-workers"}, &got))
	require.Empty(t, got.Spec.MachineRef)
	require.NotNil(t, got.Spec.MachineSelector)
	require.Equal(t, map[string]string{
		"role": "gpu",
		"site": "edge-1",
	}, got.Spec.MachineSelector.MatchLabels)
	require.Equal(t, v1alpha3.OperationNodeReboot, got.Spec.OperationKind)
	require.Nil(t, got.Spec.TTLSecondsAfterFinished)
}

func TestMachineOperationAliasSmokeCreatesOwnedOperation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newMachineOperationTestScheme(t)
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-01",
			UID:  types.UID("worker-01-uid"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	var out bytes.Buffer

	createOptions := newMachineOperationAliasCreateOptions(machineOperationAliasOptions{
		kind:              v1alpha3.OperationHostHardReboot,
		operationNamePart: "host-hard-reboot",
		machine:           "worker-01",
		operationName:     "worker-01-host-hard-reboot",
		ttlSeconds:        defaultTTLSeconds,
		wait:              false,
	}, &out, time.Date(2026, 7, 8, 15, 30, 12, 0, time.UTC))

	require.NoError(t, createOptions.validate())
	op, err := createOptions.build()
	require.NoError(t, err)
	require.NoError(t, createOptions.runWithClient(ctx, c, op))

	var got v1alpha3.MachineOperation
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "worker-01-host-hard-reboot"}, &got))
	require.Equal(t, "worker-01", got.Spec.MachineRef)
	require.Equal(t, v1alpha3.OperationHostHardReboot, got.Spec.OperationKind)
	require.NotNil(t, got.Spec.TTLSecondsAfterFinished)
	require.Equal(t, int32(defaultTTLSeconds), *got.Spec.TTLSecondsAfterFinished)
	require.Equal(t, "machineoperations/worker-01-host-hard-reboot created\n", out.String())
	require.Len(t, got.OwnerReferences, 1)
	require.Equal(t, "Machine", got.OwnerReferences[0].Kind)
	require.Equal(t, "worker-01", got.OwnerReferences[0].Name)
	require.Equal(t, types.UID("worker-01-uid"), got.OwnerReferences[0].UID)
}

func TestGenerateMachineOperationName(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 15, 30, 12, 0, time.UTC)

	require.Equal(t, "worker-01-host-hard-reboot-20260708-153012",
		generateMachineOperationName("worker-01", "host-hard-reboot", now))
}

func TestConfirmAgentReset(t *testing.T) {
	t.Parallel()

	err := confirmAgentResetWithTerminal("machine-1", strings.NewReader("machine-1\n"), ioDiscard{}, true)
	require.NoError(t, err)
}

func TestConfirmAgentResetRejectsMismatch(t *testing.T) {
	t.Parallel()

	err := confirmAgentResetWithTerminal("machine-1", strings.NewReader("wrong\n"), ioDiscard{}, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "confirmation did not match")
}

func TestConfirmAgentResetRequiresForceInNonInteractiveMode(t *testing.T) {
	t.Parallel()

	err := confirmAgentResetWithTerminal("machine-1", strings.NewReader("machine-1\n"), ioDiscard{}, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--force")
}

func TestWatchMachineOperationUsesResourceVersion(t *testing.T) {
	t.Parallel()

	c := &recordingWatchClient{}

	err := watchMachineOperationFromResourceVersion(context.Background(), c, "op-1", "12345", io.Discard)
	require.Error(t, err)
	require.Equal(t, "12345", c.resourceVersion)
	require.Equal(t, "metadata.name=op-1", c.fieldSelector)
}

func selectorOperationParent() *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade-rack-01", UID: "parent-uid"},
		Spec: v1alpha3.MachineOperationSpec{
			OperationKind: v1alpha3.OperationAgentUpgrade,
			MachineSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"topology.unbounded-cloud.io/rack": "dc-a.rack-01"},
			},
		},
	}
}

func selectorOperationWithStatus(parent *v1alpha3.MachineOperation, status v1alpha3.MachineOperationStatus) *v1alpha3.MachineOperation {
	op := parent.DeepCopy()
	op.Status = status

	return op
}

// A selector operation does not list its machines; the wait prints the
// counts of its children as they change.
func TestWaitForSelectorOperationReportsCounts(t *testing.T) {
	t.Parallel()

	parent := selectorOperationParent()
	summary := func(held, active, complete int32) *v1alpha3.MachineOperationSummary {
		return &v1alpha3.MachineOperationSummary{Total: 2, Held: held, Active: active, Complete: complete}
	}

	updates := []*v1alpha3.MachineOperation{
		selectorOperationWithStatus(parent, v1alpha3.MachineOperationStatus{
			Phase: v1alpha3.OperationPhaseInProgress, Summary: summary(1, 1, 0),
		}),
		// Nothing changed: no second line.
		selectorOperationWithStatus(parent, v1alpha3.MachineOperationStatus{
			Phase: v1alpha3.OperationPhaseInProgress, Summary: summary(1, 1, 0),
		}),
		selectorOperationWithStatus(parent, v1alpha3.MachineOperationStatus{
			Phase: v1alpha3.OperationPhaseInProgress, Summary: summary(0, 1, 1),
		}),
		selectorOperationWithStatus(parent, v1alpha3.MachineOperationStatus{
			Phase: v1alpha3.OperationPhaseComplete, Summary: summary(0, 0, 2),
		}),
	}

	var out bytes.Buffer

	c := &scriptedOperationWatchClient{initial: parent, updates: updates}

	err := waitForMachineOperation(context.Background(), c, "upgrade-rack-01", &out)
	require.NoError(t, err)

	got := out.String()
	require.Contains(t, got, "Operation AgentUpgrade: upgrade-rack-01 in progress...")
	require.Equal(t, 1, strings.Count(got, "Machines: 2 total, 1 held, 1 active, 0 complete, 0 failed"))
	require.Contains(t, got, "Machines: 2 total, 0 held, 1 active, 1 complete, 0 failed")
	require.Contains(t, got, "Machines: 2 total, 0 held, 0 active, 2 complete, 0 failed")
	require.NotContains(t, got, "Target ")
	require.Contains(t, got, "ready")
}

func TestWaitForSelectorOperationFailsWhenParentFails(t *testing.T) {
	t.Parallel()

	parent := selectorOperationParent()

	updates := []*v1alpha3.MachineOperation{
		selectorOperationWithStatus(parent, v1alpha3.MachineOperationStatus{
			Phase:   v1alpha3.OperationPhaseInProgress,
			Summary: &v1alpha3.MachineOperationSummary{Total: 12, Held: 11, Active: 1},
		}),
		selectorOperationWithStatus(parent, v1alpha3.MachineOperationStatus{
			Phase:   v1alpha3.OperationPhaseFailed,
			Message: "machines: 12 total, 0 held, 0 active, 0 complete, 12 failed",
			Summary: &v1alpha3.MachineOperationSummary{
				Total: 12, Failed: 12,
				FailedMachines: []string{"w-01", "w-02", "w-03", "w-04", "w-05", "w-06", "w-07", "w-08", "w-09", "w-10"},
			},
		}),
	}

	var out bytes.Buffer

	c := &scriptedOperationWatchClient{initial: parent, updates: updates}

	err := waitForMachineOperation(context.Background(), c, "upgrade-rack-01", &out)
	require.Error(t, err)
	require.Contains(t, err.Error(), "operation failed")
	require.Contains(t, err.Error(), "12 failed")

	got := out.String()
	require.Contains(t, got, "Failed machines: w-01, w-02, w-03, w-04, w-05, w-06, w-07, w-08, w-09, w-10 and 2 more")
	require.Contains(t, got, "kubectl get machineoperations -l unbounded-cloud.io/parent-operation-uid=parent-uid")
	require.NotContains(t, got, "ready")
}

func TestWaitForMachineRefOperationKeepsTargetLine(t *testing.T) {
	t.Parallel()

	parent := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "reboot-worker-01"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "worker-01",
			OperationKind: v1alpha3.OperationHostHardReboot,
		},
		Status: v1alpha3.MachineOperationStatus{
			Phase:   v1alpha3.OperationPhasePending,
			Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: "worker-01"}},
		},
	}

	updates := []*v1alpha3.MachineOperation{
		selectorOperationWithStatus(parent, v1alpha3.MachineOperationStatus{
			Phase:   v1alpha3.OperationPhaseInProgress,
			Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: "worker-01", Phase: v1alpha3.OperationPhaseInProgress, Message: "restarting host"}},
		}),
		selectorOperationWithStatus(parent, v1alpha3.MachineOperationStatus{
			Phase:   v1alpha3.OperationPhaseComplete,
			Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: "worker-01", Phase: v1alpha3.OperationPhaseComplete}},
		}),
	}

	var out bytes.Buffer

	c := &scriptedOperationWatchClient{initial: parent, updates: updates}

	err := waitForMachineOperation(context.Background(), c, "reboot-worker-01", &out)
	require.NoError(t, err)
	require.Contains(t, out.String(), "Target worker-01: InProgress - restarting host")
	require.NotContains(t, out.String(), "Target worker-01 (")
}

// scriptedOperationWatchClient serves one stored MachineOperation on Get and
// replays a fixed sequence of updates on Watch.
type scriptedOperationWatchClient struct {
	client.WithWatch

	initial *v1alpha3.MachineOperation
	updates []*v1alpha3.MachineOperation
}

func (c *scriptedOperationWatchClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	op, ok := obj.(*v1alpha3.MachineOperation)
	if !ok {
		return fmt.Errorf("unexpected object type %T", obj)
	}

	if key.Name != c.initial.Name {
		return fmt.Errorf("operation %s not found", key.Name)
	}

	*op = *c.initial

	return nil
}

func (c *scriptedOperationWatchClient) Watch(_ context.Context, _ client.ObjectList, _ ...client.ListOption) (watch.Interface, error) {
	w := watch.NewFakeWithChanSize(len(c.updates), false)
	for _, update := range c.updates {
		w.Modify(update)
	}

	w.Stop()

	return w, nil
}

type recordingWatchClient struct {
	client.WithWatch

	resourceVersion string
	fieldSelector   string
}

func (c *recordingWatchClient) Watch(_ context.Context, _ client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
	listOpts := (&client.ListOptions{}).ApplyOptions(opts)
	raw := listOpts.AsListOptions()

	c.resourceVersion = raw.ResourceVersion
	c.fieldSelector = raw.FieldSelector

	return watch.NewEmptyWatch(), nil
}

func newMachineOperationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(s))

	return s
}

func executeMachineOperationCreateCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	cmd := newMachineOperationCreateCommand(&machineCommandRuntime{
		newClientWithKubeconfig: func(string) (client.WithWatch, error) {
			return nil, errors.New("unexpected kube client")
		},
		commandContext: func(ctx context.Context) context.Context {
			return ctx
		},
	})
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.ExecuteContext(context.Background())

	return out.String(), err
}

// A mode change is declared in the Machine's spec.isolation and applied at the
// next repave, so there is no operation kind for it.
