// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvSliceToMap(t *testing.T) {
	t.Parallel()

	got := envSliceToMap([]string{"A=1", "B=x=y", "C=", "D"})
	require.Equal(t, map[string]string{"A": "1", "B": "x=y", "C": "", "D": ""}, got)
	require.Empty(t, envSliceToMap(nil))
}

func TestEnvMapToSlice(t *testing.T) {
	t.Parallel()

	got := envMapToSlice(map[string]string{"B": "2", "A": "1", "C": ""})
	require.Equal(t, []string{"A=1", "B=2", "C="}, got)
	require.Empty(t, envMapToSlice(nil))
}

func TestKubectlSetsKubeconfig(t *testing.T) {
	t.Parallel()

	cmd := Kubectl([]string{"PATH=/bin", "KUBECONFIG=/old"}, "/new")(t.Context())
	require.Equal(t, []string{"KUBECONFIG=/new", "PATH=/bin"}, cmd.Env)
}
