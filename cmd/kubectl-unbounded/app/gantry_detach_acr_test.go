// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/registryauth"
)

func detachFixture(t *testing.T, managed bool) (*detachACRHandler, *fake.Clientset, *[]string) {
	t.Helper()

	ns := "unbounded-system"

	data := map[string]string{"config.yaml": "upstream_registries:\n- name: central.azurecr.io\n  endpoint: https://central.azurecr.io\n"}

	if managed {
		data[registryauth.ConfigKey] = "mode: aks-identity-binding\nregistry: central.azurecr.io\nclientID: " + attachClientID + "\ntenantID: " + attachTenantID + "\nsniName: proxy.example.com\nproxyAddress: 10.0.0.1\naudience: api://test\ncaCert: |\n  " + testPEM + "\n"
	}

	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gantry-config", Namespace: ns}, Data: data}

	// The external config hash the operator stamps once the detached policy is active.
	extCfg, err := registryauth.Derive("upstream_registries: []\n", "mode: external")
	require.NoError(t, err)
	extPayload, err := json.Marshal(extCfg)
	require.NoError(t, err)

	extSum := sha256.Sum256(extPayload)
	extHash := hex.EncodeToString(extSum[:])

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry-acr-auth", Namespace: ns, Labels: map[string]string{"app.kubernetes.io/name": "gantry-acr-auth"}},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "gantry-acr-auth"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app.kubernetes.io/name": "gantry-acr-auth"},
					Annotations: map[string]string{detachHashKey: extHash},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "configure", Image: "unbounded-operator"}}},
			},
		},
	}

	ds.Generation = 1
	ds.Status.ObservedGeneration = 1
	ds.Status.DesiredNumberScheduled = 2
	ds.Status.UpdatedNumberScheduled = 2
	ds.Status.NumberAvailable = 2

	client := fake.NewClientset(source, ds)
	calls := []string{}
	h := &detachACRHandler{
		namespace: ns, timeout: 5 * time.Second, client: client, clusterID: attachClusterID,
		removeAzure: false, out: &bytes.Buffer{},
		restConfig: &rest.Config{Host: "https://test-aks-0000000000000000.azmk8s.io"},
	}
	h.runAzure = func(_ context.Context, subscriptionID string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))

		clusterID, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters")
		require.NoError(t, err)
		require.Equal(t, clusterID.SubscriptionID, subscriptionID)

		switch args[0] {
		case "aks":
			// verifyClusterMatchesKubeconfig calls `aks show`.
			require.Equal(t, "show", args[1])
			return []byte(`{"id":"` + h.clusterID + `","fqdn":"test-aks-0000000000000000.azmk8s.io","privateFqdn":""}`), nil
		case "identity":
			if args[1] == "list" {
				return []byte(`[{"id":"/subscriptions/` + subscriptionID + `/resourceGroups/test-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/gantry-pull-000000","tags":{"unbounded-cluster-id":"` + h.clusterID + `"}}]`), nil
			}

			require.Equal(t, "delete", args[1])
			require.Contains(t, args, "--name")

			return []byte("{}"), nil
		case "rest":
			require.Equal(t, "--method", args[1])
			require.Contains(t, args, "DELETE")

			return []byte("{}"), nil
		}

		t.Fatalf("unexpected az call: %v", args)

		return nil, nil
	}

	return h, client, &calls
}

func TestDetachRevertsManagedPolicyToExternal(t *testing.T) {
	h, _, _ := detachFixture(t, true)
	require.NoError(t, h.execute(context.Background()))

	cm, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(context.Background(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)

	cfg, err := registryauth.Derive(cm.Data["config.yaml"], cm.Data[registryauth.ConfigKey])
	require.NoError(t, err)
	require.Equal(t, registryauth.ExternalMode, cfg.Mode, "detach must revert the node-auth policy to external")
}

func TestDetachWithNoManagedPolicyIsNoOp(t *testing.T) {
	h, _, calls := detachFixture(t, false)
	require.NoError(t, h.execute(context.Background()))
	require.Empty(t, *calls, "detach without --remove-azure must not call az")
}

func TestDetachRemoveAzureDeletesOwnedIdentity(t *testing.T) {
	h, _, calls := detachFixture(t, true)
	h.removeAzure = true
	require.NoError(t, h.execute(context.Background()))

	joined := strings.Join(*calls, "\n")
	require.Contains(t, joined, "identity delete", "must delete the Gantry-owned pull identity")
	require.Contains(t, joined, "rest", "must delete the AKS identity binding")
}

func TestDetachRefusesRemoveAzureWithoutClusterID(t *testing.T) {
	h, _, _ := detachFixture(t, true)
	h.removeAzure = true
	h.clusterID = ""
	require.ErrorContains(t, h.execute(context.Background()), "--remove-azure requires a valid --cluster-resource-id")
}

// testPEM is a throwaway certificate used only to satisfy the node-auth policy's
// caCert field in tests; it is not a real AKS token proxy CA.
const testPEM = "-----BEGIN CERTIFICATE-----\nMIIBkTCB+wIJALJY8Q\n-----END CERTIFICATE-----\n"
