// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
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

func TestNewBootstrapStatusReporterUsesDaemonCredentials(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := baseConfig()
	machine := buildMachineCR(cfg)
	statusClient := fake.NewClientBuilder().
		WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithObjects(&machine).
		Build()

	var (
		credentialsCalled bool
		clientCalled      bool
		stopped           bool
	)

	reporter := newBootstrapStatusReporter(
		ctx,
		discardLogger(),
		cfg,
		&runOptions{
			DaemonCredentialDir: t.TempDir(),
			NewClient: func(cfg *rest.Config, _ client.Options) (client.WithWatch, error) {
				clientCalled = true

				require.Equal(t, "https://daemon-credentials.example", cfg.Host)

				return statusClient, nil
			},
		},
		func(
			context.Context,
			*slog.Logger,
			*provision.AgentConfig,
			*runOptions,
		) (*rest.Config, context.CancelFunc, error) {
			credentialsCalled = true

			return &rest.Config{Host: "https://daemon-credentials.example"}, func() {
				stopped = true
			}, nil
		},
	)

	require.True(t, reporter.ready)
	require.True(t, credentialsCalled)
	require.True(t, clientCalled)

	reporter.Succeeded(ctx)
	require.True(t, stopped)
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
