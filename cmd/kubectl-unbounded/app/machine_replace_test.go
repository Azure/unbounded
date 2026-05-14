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
