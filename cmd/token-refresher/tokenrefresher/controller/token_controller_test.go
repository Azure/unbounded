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

func TestTokenReconcilerUpdatesStaleMachineReferences(t *testing.T) {
	expired := siteToken("old123", "edge", time.Now().Add(-time.Hour))
	machines := []*unboundedv1alpha3.Machine{
		machineWithToken("machine-a", "edge", expired.Name),
		machineWithToken("machine-b", "edge", "bootstrap-token-missing"),
		machineWithToken("other-site", "other", expired.Name),
		{ObjectMeta: metav1.ObjectMeta{Name: "no-ref", Labels: map[string]string{unboundedv1alpha3.MachineSiteLabelKey: "edge"}}},
	}
	r, kubeClient := newTestReconciler(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}, expired, machines[0], machines[1], machines[2], machines[3])

	_, err := r.Reconcile(t.Context(), requestForSite("edge"))
	require.NoError(t, err)

	secrets, err := kubeClient.CoreV1().Secrets(metav1.NamespaceSystem).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secrets.Items, 2)

	var desired string

	for i := range secrets.Items {
		if secrets.Items[i].Name != expired.Name {
			desired = secrets.Items[i].Name
		}
	}

	require.NotEmpty(t, desired)

	for _, name := range []string{"machine-a", "machine-b"} {
		machine := &unboundedv1alpha3.Machine{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: name}, machine))
		require.Equal(t, desired, machine.Spec.Kubernetes.BootstrapTokenRef.Name)
	}

	other := &unboundedv1alpha3.Machine{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "other-site"}, other))
	require.Equal(t, expired.Name, other.Spec.Kubernetes.BootstrapTokenRef.Name)

	noRef := &unboundedv1alpha3.Machine{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "no-ref"}, noRef))
	require.Nil(t, noRef.Spec.Kubernetes)
}

func TestTokenReconcilerPreservesValidOlderMachineReference(t *testing.T) {
	current := siteToken("new123", "edge", time.Now().Add(2*time.Hour))
	older := siteToken("old123", "edge", time.Now().Add(time.Hour))
	machine := machineWithToken("machine-a", "edge", older.Name)
	r, _ := newTestReconciler(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}, current, older, machine)

	_, err := r.Reconcile(t.Context(), requestForSite("edge"))
	require.NoError(t, err)

	updated := &unboundedv1alpha3.Machine{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: machine.Name}, updated))
	require.Equal(t, older.Name, updated.Spec.Kubernetes.BootstrapTokenRef.Name)
}

func TestTokenReconcilerReturnsReferencedSecretFailure(t *testing.T) {
	current := siteToken("new123", "edge", time.Now().Add(time.Hour))
	machine := machineWithToken("machine-a", "edge", "bootstrap-token-old123")
	r, kubeClient := newTestReconciler(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}, current, machine)
	kubeClient.PrependReactor("get", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("secret API unavailable")
	})

	_, err := r.Reconcile(t.Context(), requestForSite("edge"))
	require.ErrorContains(t, err, "secret API unavailable")

	unchanged := &unboundedv1alpha3.Machine{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: machine.Name}, unchanged))
	require.Equal(t, "bootstrap-token-old123", unchanged.Spec.Kubernetes.BootstrapTokenRef.Name)
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
	require.Equal(t, []ctrl.Request{requestForSite("edge")}, r.requestsForSecret(t.Context(), secret))
	secret.Labels[unboundedv1alpha3.MachineSiteLabelKey] = clusterSiteName
	require.Empty(t, r.requestsForSecret(t.Context(), secret))
}

func TestRequestsForMachine(t *testing.T) {
	r := &TokenReconciler{}
	machine := machineWithToken("machine-a", "edge", "bootstrap-token-abc123")
	require.Equal(t, []ctrl.Request{requestForSite("edge")}, r.requestsForMachine(t.Context(), machine))

	machine.Labels[unboundedv1alpha3.MachineSiteLabelKey] = clusterSiteName
	require.Empty(t, r.requestsForMachine(t.Context(), machine))
}

func newTestReconciler(t *testing.T, objects ...client.Object) (*TokenReconciler, *fake.Clientset) {
	t.Helper()

	controllerObjects := make([]client.Object, 0, len(objects))

	kubeObjects := make([]runtime.Object, 0, len(objects))
	for _, obj := range objects {
		switch obj.(type) {
		case *unboundedv1alpha3.Site:
			controllerObjects = append(controllerObjects, obj)
		case *unboundedv1alpha3.Machine:
			controllerObjects = append(controllerObjects, obj)
		case *corev1.Secret:
			kubeObjects = append(kubeObjects, obj)
		}
	}

	kubeClient := fake.NewClientset(kubeObjects...)

	controllerClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(controllerObjects...).
		WithIndex(&unboundedv1alpha3.Machine{}, machineSiteField, machineSiteIndex).
		Build()

	return &TokenReconciler{
		Client:     controllerClient,
		APIReader:  controllerClient,
		KubeClient: kubeClient,
	}, kubeClient
}

func machineWithToken(name, site, secretName string) *unboundedv1alpha3.Machine {
	return &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				unboundedv1alpha3.MachineSiteLabelKey: site,
			},
		},
		Spec: unboundedv1alpha3.MachineSpec{
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				BootstrapTokenRef: &unboundedv1alpha3.LocalObjectReference{Name: secretName},
			},
		},
	}
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
