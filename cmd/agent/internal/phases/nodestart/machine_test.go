// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
)

// discardLogger returns a logger that silently drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// newFakeScheme returns a runtime.Scheme with the v1alpha3 types registered.
func newFakeScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(v1alpha3.AddToScheme(s))

	return s
}

// goalStateFor builds a minimal NodeStart goal state for tests.
func goalStateFor(machineName, bootstrapToken string, labels map[string]string, taints []string) *goalstates.NodeStart {
	return &goalstates.NodeStart{
		MachineName: machineName,
		Kubelet: goalstates.Kubelet{
			BootstrapToken:     bootstrapToken,
			APIServer:          "https://api.example.com:6443",
			CACertData:         []byte("fake-ca"),
			NodeLabels:         labels,
			RegisterWithTaints: taints,
		},
	}
}

// newSharedClientFactory returns a newClient factory that always returns the
// provided client. Use this when you need to inspect the client state after
// a task completes.
func newSharedClientFactory(c client.Client) func(*rest.Config, client.Options) (client.Client, error) {
	return func(_ *rest.Config, _ client.Options) (client.Client, error) {
		return c, nil
	}
}

// ---------------------------------------------------------------------------
// bootstrapTokenID tests
// ---------------------------------------------------------------------------

func TestBootstrapTokenID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"normal token", "abc123.secretpart", "abc123"},
		{"no dot", "abc123", "abc123"},
		{"empty", "", ""},
		{"multiple dots", "abc.def.ghi", "abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := bootstrapTokenID(tt.token)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// RegisterMachine phase tests
// ---------------------------------------------------------------------------

func TestRegisterMachine_EmptyBootstrapToken_Skips(t *testing.T) {
	t.Parallel()

	scheme := newFakeScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	gs := goalStateFor("my-node", "", nil, nil)
	task := &registerMachine{
		log:       discardLogger(),
		goalState: gs,
		newClient: newSharedClientFactory(c),
	}

	require.NoError(t, task.Do(context.Background()))

	// Confirm no Machine CR was created.
	var list v1alpha3.MachineList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Empty(t, list.Items)
}

func TestRegisterMachine_MachineAlreadyExists_Skips(t *testing.T) {
	t.Parallel()

	existing := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "my-node"},
	}

	scheme := newFakeScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	gs := goalStateFor("my-node", "tokid.secret", nil, nil)
	task := &registerMachine{
		log:       discardLogger(),
		goalState: gs,
		newClient: newSharedClientFactory(c),
	}

	require.NoError(t, task.Do(context.Background()))

	// Confirm only one Machine CR exists (the original).
	var list v1alpha3.MachineList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Len(t, list.Items, 1)
}

func TestRegisterMachine_MachineNotFound_Creates(t *testing.T) {
	t.Parallel()

	scheme := newFakeScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	gs := goalStateFor(
		"new-node",
		"abc123.secretpart",
		map[string]string{"env": "prod"},
		[]string{"dedicated=gpu:NoSchedule"},
	)
	task := &registerMachine{
		log:       discardLogger(),
		goalState: gs,
		newClient: newSharedClientFactory(c),
	}

	require.NoError(t, task.Do(context.Background()))

	var machine v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "new-node"}, &machine))
	require.NotNil(t, machine.Spec.Kubernetes)
	assert.Equal(t, "bootstrap-token-abc123", machine.Spec.Kubernetes.BootstrapTokenRef.Name)
	assert.Equal(t, map[string]string{"env": "prod"}, machine.Spec.Kubernetes.NodeLabels)
	assert.Equal(t, []string{"dedicated=gpu:NoSchedule"}, machine.Spec.Kubernetes.RegisterWithTaints)
}

func TestRegisterMachine_MachineNotFound_MinimalCreate(t *testing.T) {
	t.Parallel()

	scheme := newFakeScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	// No labels or taints.
	gs := goalStateFor("bare-node", "tid001.secret", nil, nil)
	task := &registerMachine{
		log:       discardLogger(),
		goalState: gs,
		newClient: newSharedClientFactory(c),
	}

	require.NoError(t, task.Do(context.Background()))

	var machine v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "bare-node"}, &machine))
	require.NotNil(t, machine.Spec.Kubernetes)
	assert.Equal(t, "bootstrap-token-tid001", machine.Spec.Kubernetes.BootstrapTokenRef.Name)
	assert.Empty(t, machine.Spec.Kubernetes.NodeLabels)
	assert.Empty(t, machine.Spec.Kubernetes.RegisterWithTaints)
}

func TestRegisterMachine_ClientBuildError_ReturnsError(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("injected client build error")
	gs := goalStateFor("my-node", "tok.secret", nil, nil)
	task := &registerMachine{
		log:       discardLogger(),
		goalState: gs,
		newClient: func(_ *rest.Config, _ client.Options) (client.Client, error) {
			return nil, expectedErr
		},
	}

	err := task.Do(context.Background())
	require.ErrorIs(t, err, expectedErr)
}

func TestRegisterMachine_GetError_ReturnsError(t *testing.T) {
	t.Parallel()

	scheme := newFakeScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	errClient := &errorInjectingClient{Client: base, getErr: errors.New("api server unavailable")}

	gs := goalStateFor("my-node", "tok.secret", nil, nil)
	task := &registerMachine{
		log:       discardLogger(),
		goalState: gs,
		newClient: newSharedClientFactory(errClient),
	}

	err := task.Do(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api server unavailable")
}

func TestRegisterMachine_NoMatchError_Skips(t *testing.T) {
	t.Parallel()

	// Simulate the Machine CRD not being installed: Get returns a NoKindMatchError.
	noMatchErr := &apimeta.NoKindMatchError{
		GroupKind: (&v1alpha3.Machine{}).GroupVersionKind().GroupKind(),
	}
	scheme := newFakeScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	errClient := &errorInjectingClient{Client: base, getErr: noMatchErr}

	gs := goalStateFor("my-node", "tok.secret", nil, nil)
	task := &registerMachine{
		log:       discardLogger(),
		goalState: gs,
		newClient: newSharedClientFactory(errClient),
	}

	// Should succeed (log a warning) rather than returning an error.
	require.NoError(t, task.Do(context.Background()))
}

// ---------------------------------------------------------------------------
// buildMachine tests
// ---------------------------------------------------------------------------

func TestBuildMachine_PopulatesFields(t *testing.T) {
	t.Parallel()

	gs := goalStateFor(
		"my-node",
		"tid.secretpart",
		map[string]string{"zone": "east"},
		[]string{"key=val:NoSchedule"},
	)

	task := &registerMachine{goalState: gs}
	machine := task.buildMachine("tid.secretpart")

	assert.Equal(t, "my-node", machine.Name)
	require.NotNil(t, machine.Spec.Kubernetes)
	assert.Equal(t, "bootstrap-token-tid", machine.Spec.Kubernetes.BootstrapTokenRef.Name)
	assert.Equal(t, map[string]string{"zone": "east"}, machine.Spec.Kubernetes.NodeLabels)
	assert.Equal(t, []string{"key=val:NoSchedule"}, machine.Spec.Kubernetes.RegisterWithTaints)
}

// ---------------------------------------------------------------------------
// errorInjectingClient wraps a client and injects an error on Get.
// ---------------------------------------------------------------------------

type errorInjectingClient struct {
	client.Client
	getErr error
}

func (e *errorInjectingClient) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return e.getErr
}
