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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func fakeBootstrapStatusClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(fakeScheme()).
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
