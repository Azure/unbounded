// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
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

// fakeCACertPEM is a self-signed CA certificate used only in tests so that
// the PEM validation in buildClient() passes.
const fakeCACertPEM = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIULnybQx3VI0BYI2yZi3GUr3Z3MzwwCgYIKoZIzj0EAwIw
FzEVMBMGA1UEAwwMZmFrZS10ZXN0LWNhMCAXDTI2MDQxNDA1NTIxN1oYDzIxMjYw
MzIxMDU1MjE3WjAXMRUwEwYDVQQDDAxmYWtlLXRlc3QtY2EwWTATBgcqhkjOPQIB
BggqhkjOPQMBBwNCAATuDE0L4VmwupHcW3eM5HB2yDPq06/4mcbcSyhqOrwO03Dp
7EWavVPbnpq3ftGkC3qHsC81CuN/6wAifxgDYYaJo1MwUTAdBgNVHQ4EFgQUWApU
yOSviOcKPNZtj/oOUe5r3MgwHwYDVR0jBBgwFoAUWApUyOSviOcKPNZtj/oOUe5r
3MgwDwYDVR0TAQH/BAUwAwEB/zAKBggqhkjOPQQDAgNIADBFAiBS4nPJKt8QSZct
hpnfsIFMdNXiOVD3et8iXxIvG6MM4QIhAJnTuif88s4vV5c0GeAwdulG0k3fdKyI
h47WE0g7IMhA
-----END CERTIFICATE-----`

// configFor builds a daemon Config for tests.
func configFor(machineName, bootstrapToken string, labels map[string]string, taints []string) *Config {
	return &Config{
		MachineName:        machineName,
		APIServer:          "https://api.example.com:6443",
		CACertData:         []byte(fakeCACertPEM),
		BootstrapToken:     bootstrapToken,
		NodeLabels:         labels,
		RegisterWithTaints: taints,
	}
}

// withFakeClient overrides the package-level newClientFunc for the duration of
// the test and restores it on cleanup.
func withFakeClient(t *testing.T, c client.Client) {
	t.Helper()

	orig := newClientFunc
	newClientFunc = func(_ *rest.Config, _ client.Options) (client.Client, error) {
		return c, nil
	}

	t.Cleanup(func() { newClientFunc = orig })
}

// withFailingClientBuilder overrides newClientFunc to return the given error.
func withFailingClientBuilder(t *testing.T, err error) {
	t.Helper()

	orig := newClientFunc
	newClientFunc = func(_ *rest.Config, _ client.Options) (client.Client, error) {
		return nil, err
	}

	t.Cleanup(func() { newClientFunc = orig })
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
// registerMachine tests
// ---------------------------------------------------------------------------

func TestRegisterMachine_EmptyBootstrapToken_Skips(t *testing.T) {
	t.Parallel()

	scheme := newFakeScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	withFakeClient(t, c)

	cfg := configFor("my-node", "", nil, nil)
	require.NoError(t, registerMachine(context.Background(), discardLogger(), cfg))

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
	withFakeClient(t, c)

	cfg := configFor("my-node", "tokid.secret", nil, nil)
	require.NoError(t, registerMachine(context.Background(), discardLogger(), cfg))

	// Confirm only one Machine CR exists (the original).
	var list v1alpha3.MachineList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Len(t, list.Items, 1)
}

func TestRegisterMachine_MachineNotFound_Creates(t *testing.T) {
	t.Parallel()

	scheme := newFakeScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	withFakeClient(t, c)

	cfg := configFor(
		"new-node",
		"abc123.secretpart",
		map[string]string{"env": "prod"},
		[]string{"dedicated=gpu:NoSchedule"},
	)
	require.NoError(t, registerMachine(context.Background(), discardLogger(), cfg))

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
	withFakeClient(t, c)

	// No labels or taints.
	cfg := configFor("bare-node", "tid001.secret", nil, nil)
	require.NoError(t, registerMachine(context.Background(), discardLogger(), cfg))

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
	withFailingClientBuilder(t, expectedErr)

	cfg := configFor("my-node", "tok.secret", nil, nil)
	err := registerMachine(context.Background(), discardLogger(), cfg)
	require.ErrorIs(t, err, expectedErr)
}

func TestRegisterMachine_GetError_ReturnsError(t *testing.T) {
	t.Parallel()

	scheme := newFakeScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	errClient := &errorInjectingClient{Client: base, getErr: errors.New("api server unavailable")}
	withFakeClient(t, errClient)

	cfg := configFor("my-node", "tok.secret", nil, nil)
	err := registerMachine(context.Background(), discardLogger(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api server unavailable")
}

func TestRegisterMachine_NoMatchError_ReturnsError(t *testing.T) {
	t.Parallel()

	// Simulate the Machine CRD not being installed: Get returns a NoKindMatchError.
	noMatchErr := &apimeta.NoKindMatchError{
		GroupKind: (&v1alpha3.Machine{}).GroupVersionKind().GroupKind(),
	}
	scheme := newFakeScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	errClient := &errorInjectingClient{Client: base, getErr: noMatchErr}
	withFakeClient(t, errClient)

	cfg := configFor("my-node", "tok.secret", nil, nil)
	err := registerMachine(context.Background(), discardLogger(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "machine CRD is not installed")
}

func TestRegisterMachine_CreateAlreadyExists_Tolerates(t *testing.T) {
	t.Parallel()

	// Simulate a race: GET returns NotFound, but another client creates the
	// Machine CR before our CREATE arrives, so CREATE returns AlreadyExists.
	scheme := newFakeScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	errClient := &errorInjectingClient{
		Client:    base,
		createErr: apierrors.NewAlreadyExists(schema.GroupResource{Group: "unbounded-kube.io", Resource: "machines"}, "my-node"),
	}
	withFakeClient(t, errClient)

	cfg := configFor("my-node", "tok.secret", nil, nil)
	// Should succeed rather than returning an error.
	require.NoError(t, registerMachine(context.Background(), discardLogger(), cfg))
}

func TestRegisterMachine_EmptyCACertData_ReturnsError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		MachineName:    "my-node",
		APIServer:      "https://api.example.com:6443",
		CACertData:     []byte{},
		BootstrapToken: "tok.secret",
	}

	err := registerMachine(context.Background(), discardLogger(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CA certificate data is empty")
}

func TestRegisterMachine_InvalidPEMCACertData_ReturnsError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		MachineName:    "my-node",
		APIServer:      "https://api.example.com:6443",
		CACertData:     []byte("not-valid-pem-data"),
		BootstrapToken: "tok.secret",
	}

	err := registerMachine(context.Background(), discardLogger(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not contain a valid PEM block")
}

// ---------------------------------------------------------------------------
// buildMachine tests
// ---------------------------------------------------------------------------

func TestBuildMachine_PopulatesFields(t *testing.T) {
	t.Parallel()

	cfg := configFor(
		"my-node",
		"tid.secretpart",
		map[string]string{"zone": "east"},
		[]string{"key=val:NoSchedule"},
	)

	machine := buildMachine(cfg)

	assert.Equal(t, "my-node", machine.Name)
	require.NotNil(t, machine.Spec.Kubernetes)
	assert.Equal(t, "bootstrap-token-tid", machine.Spec.Kubernetes.BootstrapTokenRef.Name)
	assert.Equal(t, map[string]string{"zone": "east"}, machine.Spec.Kubernetes.NodeLabels)
	assert.Equal(t, []string{"key=val:NoSchedule"}, machine.Spec.Kubernetes.RegisterWithTaints)
}

// ---------------------------------------------------------------------------
// errorInjectingClient wraps a client and injects errors on Get and Create.
// ---------------------------------------------------------------------------

type errorInjectingClient struct {
	client.Client
	getErr    error
	createErr error
}

func (e *errorInjectingClient) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return e.getErr
}

func (e *errorInjectingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if e.createErr != nil {
		return e.createErr
	}

	return e.Client.Create(ctx, obj, opts...)
}
