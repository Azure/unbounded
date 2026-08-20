// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestTokenReconcilerCreatesMissingToken(t *testing.T) {
	r, kubeClient := newTestReconciler(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}})

	result, err := r.Reconcile(t.Context(), requestForSite("edge"))
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)

	secrets, err := kubeClient.CoreV1().Secrets(metav1.NamespaceSystem).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secrets.Items, 1)

	secret := secrets.Items[0]
	require.Equal(t, corev1.SecretTypeBootstrapToken, secret.Type)
	require.Equal(t, "edge", secret.Labels[unboundedv1alpha3.MachineSiteLabelKey])
	require.Len(t, secret.Data["token-id"], 6)
	require.Len(t, secret.Data["token-secret"], 16)
	require.Equal(t, "true", string(secret.Data["usage-bootstrap-authentication"]))
	require.Equal(t, "true", string(secret.Data["usage-bootstrap-signing"]))
	require.Equal(t, "system:bootstrappers:unbounded-agent-daemons", string(secret.Data["auth-extra-groups"]))
}

func TestTokenReconcilerKeepsValidToken(t *testing.T) {
	secret := siteToken("abc123", "edge", time.Now().Add(time.Hour))
	r, kubeClient := newTestReconciler(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}, secret)

	result, err := r.Reconcile(t.Context(), requestForSite("edge"))
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.LessOrEqual(t, result.RequeueAfter, time.Hour)

	secrets, err := kubeClient.CoreV1().Secrets(metav1.NamespaceSystem).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secrets.Items, 1)
	require.Equal(t, "bootstrap-token-abc123", secrets.Items[0].Name)
}

func TestTokenReconcilerReplacesInvalidTokens(t *testing.T) {
	tests := map[string]*corev1.Secret{
		"expired":    siteToken("abc123", "edge", time.Now().Add(-time.Hour)),
		"wrong type": siteToken("abc123", "edge", time.Now().Add(time.Hour)),
		"incomplete": siteToken("abc123", "edge", time.Now().Add(time.Hour)),
	}
	tests["wrong type"].Type = corev1.SecretTypeOpaque
	delete(tests["incomplete"].Data, "token-secret")

	for name, secret := range tests {
		t.Run(name, func(t *testing.T) {
			r, kubeClient := newTestReconciler(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}, secret)

			_, err := r.Reconcile(t.Context(), requestForSite("edge"))
			require.NoError(t, err)

			secrets, err := kubeClient.CoreV1().Secrets(metav1.NamespaceSystem).List(t.Context(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, secrets.Items, 2)
		})
	}
}

func TestTokenReconcilerIgnoresClusterAndMissingSites(t *testing.T) {
	r, kubeClient := newTestReconciler(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: clusterSiteName}})

	for _, site := range []string{clusterSiteName, "missing"} {
		result, err := r.Reconcile(t.Context(), requestForSite(site))
		require.NoError(t, err)
		require.Zero(t, result)
	}

	secrets, err := kubeClient.CoreV1().Secrets(metav1.NamespaceSystem).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, secrets.Items)
}

func TestTokenReconcilerIgnoresDisabledSite(t *testing.T) {
	disabled := false
	r, kubeClient := newTestReconciler(t, &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "edge"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
			TokenRefresher: &unboundedv1alpha3.TokenRefresherComponentSpec{
				SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &disabled},
			},
		}},
	})

	result, err := r.Reconcile(t.Context(), requestForSite("edge"))
	require.NoError(t, err)
	require.Zero(t, result)

	secrets, err := kubeClient.CoreV1().Secrets(metav1.NamespaceSystem).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, secrets.Items)
}

func TestTokenReconcilerReturnsTokenListFailure(t *testing.T) {
	r, kubeClient := newTestReconciler(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}})
	kubeClient.PrependReactor("list", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("API unavailable")
	})

	_, err := r.Reconcile(t.Context(), requestForSite("edge"))
	require.ErrorContains(t, err, "API unavailable")
}

func TestRequestsForSecret(t *testing.T) {
	r := &TokenReconciler{}
	secret := siteToken("abc123", "edge", time.Now().Add(time.Hour))

	requests := r.requestsForSecret(t.Context(), secret)
	require.Equal(t, []ctrl.Request{requestForSite("edge")}, requests)

	secret.Type = corev1.SecretTypeOpaque
	require.Empty(t, r.requestsForSecret(t.Context(), secret))
	secret.Type = corev1.SecretTypeBootstrapToken
	secret.Labels[unboundedv1alpha3.MachineSiteLabelKey] = clusterSiteName
	require.Empty(t, r.requestsForSecret(t.Context(), secret))
}

func newTestReconciler(t *testing.T, objects ...client.Object) (*TokenReconciler, *fake.Clientset) {
	t.Helper()

	controllerObjects := make([]client.Object, 0, len(objects))

	kubeObjects := make([]runtime.Object, 0, len(objects))
	for _, obj := range objects {
		switch obj.(type) {
		case *unboundedv1alpha3.Site:
			controllerObjects = append(controllerObjects, obj)
		case *corev1.Secret:
			kubeObjects = append(kubeObjects, obj)
		}
	}

	kubeClient := fake.NewClientset(kubeObjects...)

	return &TokenReconciler{
		Client:     clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(controllerObjects...).Build(),
		KubeClient: kubeClient,
	}, kubeClient
}

func requestForSite(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
}

func siteToken(id, site string, expiration time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-" + id,
			Namespace: metav1.NamespaceSystem,
			Labels: map[string]string{
				unboundedv1alpha3.MachineSiteLabelKey: site,
			},
		},
		Type: corev1.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			"token-id":     []byte(id),
			"token-secret": []byte("0123456789abcdef"),
			"expiration":   []byte(expiration.UTC().Format(time.RFC3339)),
		},
	}
}
