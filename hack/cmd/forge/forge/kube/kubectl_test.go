// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKubectlSetsKubeconfig(t *testing.T) {
	t.Parallel()

	env := []string{"KUBECONFIG=/old", "PATH=/bin"}
	kubectl := Kubectl(env, "/new")

	require.Equal(t, []string{"PATH=/bin", "KUBECONFIG=/new"}, kubectl(t.Context()).Env)
	require.Equal(t, []string{"KUBECONFIG=/old", "PATH=/bin"}, env, "the caller's environment is not modified")
	require.Equal(t, []string{"PATH=/bin", "KUBECONFIG=/new"}, kubectl(t.Context()).Env, "a second command gets the same environment")
}
