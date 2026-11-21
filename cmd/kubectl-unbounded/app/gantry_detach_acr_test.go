// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/internal/registryauth"
)

func detachFixture(t *testing.T) (*detachACRHandler, *fake.Clientset) {
	t.Helper()

	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry-config", Namespace: "custom-system"},
		Data: map[string]string{
			"config.yaml":          "# retain comment\nupstream_registries:\n- name: ghcr.io\n  endpoint: https://ghcr.io\n",
			registryauth.ConfigKey: "mode: aks-identity-binding\nregistry: central.azurecr.io\n",
		},
	}
	client := fake.NewClientset(source)
	h := &detachACRHandler{
		namespace: source.Namespace, timeout: 5 * time.Second, client: client, out: &bytes.Buffer{},
	}

	return h, client
}

func TestGantryDetachACRRevertsPolicyToExternal(t *testing.T) {
	h, client := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{"off-azure": true}}

	require.NoError(t, h.execute(t.Context()))

	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)

	cfg, err := registryauth.Derive(source.Data["config.yaml"], source.Data[registryauth.ConfigKey])
	require.NoError(t, err)
	require.Equal(t, registryauth.ExternalMode, cfg.Mode)
}

func TestGantryDetachACRWaitTimesOutWhenNodesUnconverged(t *testing.T) {
	h, _ := detachFixture(t)
	h.timeout = 2 * time.Second

	polls := 0
	h.machineClient = machineStatusReaderFunc(func(context.Context) (map[string]bool, error) {
		polls++

		return map[string]bool{"off-azure": false}, nil
	})

	require.ErrorContains(t, h.execute(t.Context()), "timed out waiting for Flex nodes")
	require.GreaterOrEqual(t, polls, 1)
}

func TestGantryDetachACRRemoveGrantRequiresClusterID(t *testing.T) {
	h, _ := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = ""

	require.ErrorContains(t, h.execute(t.Context()), "--cluster-resource-id is required with --remove-azure")
}

func TestGantryDetachACRRemoveGrantIsOwnershipGated(t *testing.T) {
	h, client := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = attachClusterID

	// An unowned grant must be left in place.
	_, err := client.RbacV1().ClusterRoleBindings().Create(t.Context(), &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: registryauth.IdentityRoleName, Labels: map[string]string{"app.kubernetes.io/managed-by": "customer"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	require.ErrorContains(t, h.execute(t.Context()), "not operator-owned")

	binding, err := client.RbacV1().ClusterRoleBindings().Get(t.Context(), registryauth.IdentityRoleName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "customer", binding.Labels["app.kubernetes.io/managed-by"], "an unowned grant must survive detach")
}
