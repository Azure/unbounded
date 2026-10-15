// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
)

func fakeBootstrapStatusClient(objs ...client.Object) client.Client {
	scheme := fakeScheme()
	_ = corev1.AddToScheme(scheme)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithObjects(objs...).
		Build()
}

func TestBootstrapStatusReporter_RunningSetsCondition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := baseConfig()
	machine := buildMachineCR(cfg)
	reporter := &BootstrapStatusReporter{
		log:     discardLogger(),
		client:  fakeBootstrapStatusClient(&machine),
		machine: cfg.MachineName,
		ready:   true,
	}

	reporter.Running(ctx)

	var got v1alpha3.Machine
	require.NoError(t, reporter.client.Get(ctx, client.ObjectKey{Name: cfg.MachineName}, &got))
	cond := findMachineCondition(t, got.Status.Conditions, v1alpha3.MachineConditionAgentBootstrapped)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "Running", cond.Reason)
	assert.Equal(t, "unbounded-agent bootstrap is running", cond.Message)
}

func TestBootstrapStatusReporter_FailedTruncatesMessage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := baseConfig()
	machine := buildMachineCR(cfg)
	reporter := &BootstrapStatusReporter{
		log:     discardLogger(),
		client:  fakeBootstrapStatusClient(&machine),
		machine: cfg.MachineName,
		ready:   true,
	}

	reporter.Failed(ctx, "KubeletBootstrapFailed", errors.New(strings.Repeat("x", bootstrapConditionMessageMaxLen+100)))

	var got v1alpha3.Machine
	require.NoError(t, reporter.client.Get(ctx, client.ObjectKey{Name: cfg.MachineName}, &got))
	cond := findMachineCondition(t, got.Status.Conditions, v1alpha3.MachineConditionAgentBootstrapped)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "KubeletBootstrapFailed", cond.Reason)
	assert.Len(t, cond.Message, bootstrapConditionMessageMaxLen)
	assert.True(t, strings.HasSuffix(cond.Message, "..."))
}

func TestBootstrapStatusReporter_SucceededSetsCondition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := baseConfig()
	machine := buildMachineCR(cfg)
	reporter := &BootstrapStatusReporter{
		log:     discardLogger(),
		client:  fakeBootstrapStatusClient(&machine),
		machine: cfg.MachineName,
		ready:   true,
	}

	reporter.Succeeded(ctx)

	var got v1alpha3.Machine
	require.NoError(t, reporter.client.Get(ctx, client.ObjectKey{Name: cfg.MachineName}, &got))
	cond := findMachineCondition(t, got.Status.Conditions, v1alpha3.MachineConditionAgentBootstrapped)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Succeeded", cond.Reason)
	assert.Equal(t, "unbounded-agent bootstrap completed successfully", cond.Message)
}

func TestBootstrapStatusReporter_SucceededSetsIMEXPreparedCondition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := baseConfig()
	cfg.NVIDIA = &provision.NVIDIAConfig{
		IMEX: &provision.NVIDIAIMEXConfig{Mode: provision.NVIDIAIMEXModeDRA},
	}
	machine := buildMachineCR(cfg)
	reporter := &BootstrapStatusReporter{
		log:     discardLogger(),
		client:  fakeBootstrapStatusClient(&machine),
		machine: cfg.MachineName,
		ready:   true,
		imexDRA: true,
	}

	reporter.Succeeded(ctx)

	var got v1alpha3.Machine
	require.NoError(t, reporter.client.Get(ctx, client.ObjectKey{Name: cfg.MachineName}, &got))
	cond := findMachineCondition(t, got.Status.Conditions, v1alpha3.MachineConditionNVIDIAIMEXPrepared)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "HostQualified", cond.Reason)
	assert.Contains(t, cond.Message, "does not assert ComputeDomain readiness")
}

func TestBootstrapStatusReporter_DisabledClearsIMEXPreparedCondition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := baseConfig()
	machine := buildMachineCR(cfg)
	machine.Status.Conditions = append(machine.Status.Conditions, metav1.Condition{
		Type:   v1alpha3.MachineConditionNVIDIAIMEXPrepared,
		Status: metav1.ConditionTrue,
		Reason: "Prepared",
	})
	reporter := &BootstrapStatusReporter{
		log:     discardLogger(),
		client:  fakeBootstrapStatusClient(&machine),
		machine: cfg.MachineName,
		ready:   true,
	}

	reporter.Succeeded(ctx)

	var got v1alpha3.Machine
	require.NoError(t, reporter.client.Get(ctx, client.ObjectKey{Name: cfg.MachineName}, &got))

	for _, condition := range got.Status.Conditions {
		assert.NotEqual(t, v1alpha3.MachineConditionNVIDIAIMEXPrepared, condition.Type)
	}
}

func TestSetNVIDIAIMEXPreparedCondition(t *testing.T) {
	t.Parallel()

	machine := v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Generation: 7}}
	setNVIDIAIMEXPreparedCondition(&machine, true)

	condition := findMachineCondition(t, machine.Status.Conditions, v1alpha3.MachineConditionNVIDIAIMEXPrepared)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, int64(7), condition.ObservedGeneration)
	assert.Contains(t, condition.Message, "does not assert ComputeDomain readiness")

	setNVIDIAIMEXPreparedCondition(&machine, false)

	for _, condition := range machine.Status.Conditions {
		assert.NotEqual(t, v1alpha3.MachineConditionNVIDIAIMEXPrepared, condition.Type)
	}
}

func TestSetNVIDIAIMEXQualificationFailedCondition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	machine := v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "gb200", Generation: 9}}
	kubeClient := fakeBootstrapStatusClient(&machine)

	require.NoError(t, setNVIDIAIMEXQualificationFailedCondition(
		ctx,
		kubeClient,
		machine.Name,
		errors.New("qualification digest mismatch"),
	))

	var got v1alpha3.Machine
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Name: machine.Name}, &got))
	condition := findMachineCondition(t, got.Status.Conditions, v1alpha3.MachineConditionNVIDIAIMEXPrepared)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "HostQualificationFailed", condition.Reason)
	assert.Equal(t, "qualification digest mismatch", condition.Message)
	assert.Equal(t, int64(9), condition.ObservedGeneration)
}

func TestSyncNVIDIAIMEXNodeMetadata(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gb200",
			Labels: map[string]string{
				provision.NVIDIAIMEXModeLabelKey:           string(provision.NVIDIAIMEXModeDRA),
				provision.NVIDIAHostProfileLabelKey:        "old-profile",
				provision.NVIDIAHostProfileVersionLabelKey: "v0",
			},
			Annotations: map[string]string{
				provision.NVIDIAHostProfileDigestAnnotationKey: "sha256:old",
			},
		},
	}
	kubeClient := fakeBootstrapStatusClient(node)

	require.NoError(t, syncNVIDIAIMEXNodeMetadata(ctx, kubeClient, node.Name, nil))

	var got corev1.Node
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Name: node.Name}, &got))
	assert.NotContains(t, got.Labels, provision.NVIDIAIMEXModeLabelKey)
	assert.NotContains(t, got.Labels, provision.NVIDIAHostProfileLabelKey)
	assert.NotContains(t, got.Labels, provision.NVIDIAHostProfileVersionLabelKey)
	assert.NotContains(t, got.Annotations, provision.NVIDIAHostProfileDigestAnnotationKey)

	imex := &provision.NVIDIAIMEXConfig{
		Mode:                provision.NVIDIAIMEXModeDRA,
		HostProfile:         "hillsboro-gb200-gpu-operator-ready",
		HostProfileVersion:  "v2026.10.14-1",
		HostContractVersion: "hillsboro.nvidia-host-capabilities/v1",
		HostArtifactPath:    "/etc/unbounded/host-capabilities/nvidia-gpu-operator.json",
		HostProfileDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	require.NoError(t, syncNVIDIAIMEXNodeMetadata(ctx, kubeClient, node.Name, imex))
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Name: node.Name}, &got))
	assert.Equal(t, string(provision.NVIDIAIMEXModeDRA), got.Labels[provision.NVIDIAIMEXModeLabelKey])
	assert.Equal(t, imex.HostProfile, got.Labels[provision.NVIDIAHostProfileLabelKey])
	assert.Equal(t, "v2026-10-14-1", got.Labels[provision.NVIDIAHostProfileVersionLabelKey])
	assert.Equal(t, imex.HostProfileDigest, got.Annotations[provision.NVIDIAHostProfileDigestAnnotationKey])

	require.NoError(t, syncNVIDIAIMEXNodeMetadata(ctx, kubeClient, node.Name, imex))
}

func findMachineCondition(t *testing.T, conditions []metav1.Condition, conditionType string) metav1.Condition {
	t.Helper()

	for _, cond := range conditions {
		if cond.Type == conditionType {
			return cond
		}
	}

	t.Fatalf("condition %q not found", conditionType)

	return metav1.Condition{}
}

// TestBootstrapStatusReporter_NilIsSafe pins that every reporting entry point
// tolerates a nil receiver.
//
// Bootstrap builds the reporter only once attestation has supplied credentials,
// so stages that run before that, and a failure on one of them, report through a
// nil reporter. The callers rely on this instead of guarding each call, so it is
// a contract rather than an accident.
func TestBootstrapStatusReporter_NilIsSafe(t *testing.T) {
	t.Parallel()

	var reporter *BootstrapStatusReporter

	assert.NotPanics(t, func() { reporter.Running(context.Background()) })
	assert.NotPanics(t, func() { reporter.Failed(context.Background(), "Failed", errors.New("boom")) })
	assert.NotPanics(t, func() { reporter.Failed(context.Background(), "Failed", nil) })
	assert.NotPanics(t, func() { reporter.Succeeded(context.Background()) })
}
