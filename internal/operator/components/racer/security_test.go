// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestOperatorControllerSecurity(t *testing.T) {
	env := testEnv(t, cache("cache"))
	initialize(t, env)

	deployment := &appsv1.Deployment{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
	pod := deployment.Spec.Template.Spec
	require.True(t, *pod.SecurityContext.RunAsNonRoot)
	require.Equal(t, int64(65532), *pod.SecurityContext.RunAsUser)
	require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)
	container := pod.Containers[0]
	require.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	require.True(t, *container.SecurityContext.ReadOnlyRootFilesystem)
	require.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)
	require.Empty(t, container.SecurityContext.Capabilities.Add)

	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, limit := container.Resources.Requests[name], container.Resources.Limits[name]
		require.True(t, request.Sign() > 0)
		require.True(t, limit.Cmp(request) >= 0)
	}
}
