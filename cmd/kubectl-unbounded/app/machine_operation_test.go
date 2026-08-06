// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
		kind:       v1alpha3.OperationHostReboot,
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
	require.Equal(t, v1alpha3.OperationHostReboot, op.Spec.OperationKind)
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
		parameterArgs: []string{"downloadURL=https://example.com/agent.tar.gz"},
		output:        operationOutputName,
		dryRun:        dryRunNone,
	}

	err := opts.validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "sha256")
}

func TestValidateWaitRejectsStructuredOutput(t *testing.T) {
	t.Parallel()

	for _, output := range []string{operationOutputYAML, operationOutputJSON} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()

			opts := &machineOperationCreateOptions{
				name:    "reboot-worker-01",
				kind:    v1alpha3.OperationHostReboot,
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

func TestMachineOperationCreateCommandRejectsNegativeTTLFlag(t *testing.T) {
	t.Parallel()

	out, err := executeMachineOperationCreateCommand(
		t,
		"reboot-worker-01",
		"--kind", string(v1alpha3.OperationHostReboot),
		"--machine", "worker-01",
		"--ttl=-1",
		"--dry-run=client",
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "--ttl must be 0 or greater")
	require.Empty(t, out)
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
		kind:              v1alpha3.OperationHostReboot,
		operationNamePart: "host-reboot",
		machine:           "worker-01",
		operationName:     "worker-01-host-reboot",
		ttlSeconds:        defaultTTLSeconds,
		wait:              false,
	}, &out, time.Date(2026, 7, 8, 15, 30, 12, 0, time.UTC))

	require.NoError(t, createOptions.validate())
	op, err := createOptions.build()
	require.NoError(t, err)
	require.NoError(t, createOptions.runWithClient(ctx, c, op))

	var got v1alpha3.MachineOperation
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "worker-01-host-reboot"}, &got))
	require.Equal(t, "worker-01", got.Spec.MachineRef)
	require.Equal(t, v1alpha3.OperationHostReboot, got.Spec.OperationKind)
	require.NotNil(t, got.Spec.TTLSecondsAfterFinished)
	require.Equal(t, int32(defaultTTLSeconds), *got.Spec.TTLSecondsAfterFinished)
	require.Equal(t, "machineoperations/worker-01-host-reboot created\n", out.String())
	require.Len(t, got.OwnerReferences, 1)
	require.Equal(t, "Machine", got.OwnerReferences[0].Kind)
	require.Equal(t, "worker-01", got.OwnerReferences[0].Name)
	require.Equal(t, types.UID("worker-01-uid"), got.OwnerReferences[0].UID)
}

func TestGenerateMachineOperationName(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 15, 30, 12, 0, time.UTC)

	require.Equal(t, "worker-01-host-reboot-20260708-153012",
		generateMachineOperationName("worker-01", "host-reboot", now))
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
