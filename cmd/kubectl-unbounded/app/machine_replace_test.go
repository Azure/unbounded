// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestRunReplaceRequiresForceInNonInteractiveMode(t *testing.T) {
	t.Parallel()

	s := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(s))

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	err := runReplace(context.Background(), c, "machine-1", 0, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--force")
}

func TestRunReplaceCreatesNamedOperationWithoutWaiting(t *testing.T) {
	t.Parallel()

	s := runtime.NewScheme()
	require.NoError(t, v1alpha3.AddToScheme(s))

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()

	var err error

	_ = captureStdout(t, func() {
		err = runReplaceWithOptions(context.Background(), c, "machine-1", machineReplaceOptions{
			force:         true,
			wait:          false,
			operationName: "replace-machine-1",
		})
	})
	require.NoError(t, err)

	var op v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "replace-machine-1"}, &op))
	require.Equal(t, "machine-1", op.Spec.MachineRef)
	require.Equal(t, v1alpha3.OperationHostReplace, op.Spec.OperationKind)
	require.Len(t, op.OwnerReferences, 1)
	require.Equal(t, "machine-1", op.OwnerReferences[0].Name)
}

func TestConfirmReplace(t *testing.T) {
	t.Parallel()

	err := confirmReplaceWithTerminal("machine-1", strings.NewReader("machine-1\n"), ioDiscard{}, true)
	require.NoError(t, err)
}

func TestConfirmReplaceRejectsMismatch(t *testing.T) {
	t.Parallel()

	err := confirmReplaceWithTerminal("machine-1", strings.NewReader("wrong\n"), ioDiscard{}, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "confirmation did not match")
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
