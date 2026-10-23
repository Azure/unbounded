// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
)

// racerPod is one dataplane Pod and a client that authenticates as it, the
// way a bound projected token would.
type racerPod struct {
	pod    *corev1.Pod
	client client.Client
}

type admissionFixture struct {
	admin client.Client
	env   *component.Env
	a, b  racerPod
}

func startAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()

	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run the Racer admission policy and RBAC tests against a real API server")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	admin, err := client.New(rc, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	env := &component.Env{Client: admin, APIReader: admin, Scheme: scheme.Scheme, Namespace: "custom-system"}
	ctx := t.Context()
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.Namespace}}))

	objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"policies.yaml", "rbac.yaml", "control.yaml", "mutating-webhook.yaml"}, nil)
	require.NoError(t, err)

	for _, obj := range objects {
		require.NoError(t, admin.Create(ctx, obj), "%s %s", obj.GetKind(), obj.GetName())
	}

	require.NoError(t, admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: env.Namespace}}))

	plan := component.NewPlan()
	require.NoError(t, planEndpointSlice(ctx, env, plan))
	require.Equal(t, 1, plan.Len())
	require.NoError(t, admin.Create(ctx, plan.Operations[0].Object))

	fixture := &admissionFixture{admin: admin, env: env}
	fixture.a = fixture.pod(t, rc, "racer-dataplane-a", "node-a")
	fixture.b = fixture.pod(t, rc, "racer-dataplane-b", "node-b")

	// Policies compile asynchronously. Wait until a foreign Pod write is denied.
	require.Eventually(t, func() bool {
		err := fixture.annotatePod(ctx, fixture.a.client, fixture.b.pod, "racer.unbounded-cloud.io/report", "{}")
		return apierrors.IsInvalid(err) || apierrors.IsForbidden(err)
	}, 30*time.Second, 200*time.Millisecond)

	return fixture
}

func (f *admissionFixture) pod(t *testing.T, rc *rest.Config, name, node string) racerPod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.env.Namespace, Labels: map[string]string{"app": "racer"}},
		Spec: corev1.PodSpec{
			ServiceAccountName: dataplaneName, AutomountServiceAccountToken: ptr.To(false), NodeName: node,
			Containers: []corev1.Container{{Name: "dataplane", Image: "racer:test"}},
		},
	}
	require.NoError(t, f.admin.Create(t.Context(), pod))

	impersonated := rest.CopyConfig(rc)
	impersonated.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + f.env.Namespace + ":" + dataplaneName,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + f.env.Namespace, "system:authenticated"},
		Extra: map[string][]string{
			"authentication.kubernetes.io/pod-name":  {name},
			"authentication.kubernetes.io/pod-uid":   {string(pod.UID)},
			"authentication.kubernetes.io/node-name": {node},
		},
	}

	c, err := client.New(impersonated, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	return racerPod{pod: pod, client: c}
}

func (f *admissionFixture) annotatePod(ctx context.Context, c client.Client, pod *corev1.Pod, key, value string) error {
	patch := client.MergeFrom(pod.DeepCopy())
	updated := pod.DeepCopy()

	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}

	updated.Annotations[key] = value

	return c.Patch(ctx, updated, patch)
}

func denied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.True(t, apierrors.IsForbidden(err) || apierrors.IsInvalid(err), "want denial, got %v", err)
}

// update reads the object fresh, mutates it, and writes it as the caller.
func update[T client.Object](t *testing.T, f *admissionFixture, as client.Client, obj T, mutate func(T)) error {
	t.Helper()
	require.NoError(t, f.admin.Get(t.Context(), client.ObjectKeyFromObject(obj), obj))
	mutate(obj)

	return as.Update(t.Context(), obj)
}

// patch reads the object fresh, mutates it, and merge-patches it as the caller.
func patch[T client.Object](t *testing.T, f *admissionFixture, as client.Client, obj T, mutate func(T)) error {
	t.Helper()
	require.NoError(t, f.admin.Get(t.Context(), client.ObjectKeyFromObject(obj), obj))

	base, ok := obj.DeepCopyObject().(client.Object)
	require.True(t, ok)
	mutate(obj)

	return as.Patch(t.Context(), obj, client.MergeFrom(base))
}

func TestEnvtestRacerAdmissionAndRBAC(t *testing.T) {
	f := startAdmissionFixture(t)
	ctx := t.Context()
	ns := f.env.Namespace
	a, b := f.a, f.b

	t.Run("pod report", func(t *testing.T) {
		require.NoError(t, f.annotatePod(ctx, a.client, a.pod, "racer.unbounded-cloud.io/report", `{"shares":4}`))
		denied(t, f.annotatePod(ctx, a.client, b.pod, "racer.unbounded-cloud.io/report", `{"shares":4}`))
		denied(t, f.annotatePod(ctx, a.client, a.pod, "other", "x"))

		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: a.pod.Name, Namespace: ns}}
		denied(t, update(t, f, a.client, pod, func(p *corev1.Pod) { p.Labels["app"] = "changed" }))
		denied(t, update(t, f, a.client, pod, func(p *corev1.Pod) { p.Finalizers = []string{"example.com/hold"} }))
		denied(t, update(t, f, a.client, pod, func(p *corev1.Pod) { p.Spec.ActiveDeadlineSeconds = ptr.To(int64(5)) }))
		denied(t, update(t, f, a.client, pod, func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "x"}}
		}))
		// Matches MAX_REPORT_BYTES in cmd/racer-dataplane/cluster-control/src/limits.rs.
		const maxReport = 32 * 1024
		require.NoError(t, f.annotatePod(ctx, a.client, a.pod, "racer.unbounded-cloud.io/report", strings.Repeat("r", maxReport)))
		denied(t, f.annotatePod(ctx, a.client, a.pod, "racer.unbounded-cloud.io/report", strings.Repeat("r", maxReport+1)))
		denied(t, update(t, f, a.client, pod, func(p *corev1.Pod) {
			p.Annotations["racer.unbounded-cloud.io/report"] = strings.Repeat("r", maxReport+1)
		}))
		denied(t, a.client.Delete(ctx, b.pod))
		denied(t, a.client.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "extra", Namespace: ns}, Spec: b.pod.Spec}))
	})

	t.Run("leases", func(t *testing.T) {
		seat := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "racer-seat-0", Namespace: ns}}
		claim := func(p racerPod, transitions int32) func(*coordinationv1.Lease) {
			return func(l *coordinationv1.Lease) {
				l.Spec.HolderIdentity = ptr.To(p.pod.Name)
				l.Spec.LeaseDurationSeconds = ptr.To(int32(15))
				l.Spec.LeaseTransitions = ptr.To(transitions)
				l.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}

				if l.Annotations == nil {
					l.Annotations = map[string]string{}
				}

				l.Annotations["racer.unbounded-cloud.io/holder-uid"] = string(p.pod.UID)
				l.Annotations["racer.unbounded-cloud.io/holder-node"] = p.pod.Spec.NodeName
				l.Annotations["racer.unbounded-cloud.io/endpoint"] = "https://10.0.0.1:8443"
				l.Annotations["racer.unbounded-cloud.io/nonce"] = string(p.pod.UID)
			}
		}

		// Claims must name the caller and start a new term.
		denied(t, update(t, f, a.client, seat, claim(b, 1)))
		denied(t, update(t, f, a.client, seat, claim(a, 0)))
		denied(t, update(t, f, a.client, seat, func(l *coordinationv1.Lease) {
			claim(a, 1)(l)
			l.Annotations["racer.unbounded-cloud.io/holder-uid"] = string(b.pod.UID)
		}))
		denied(t, update(t, f, a.client, seat, func(l *coordinationv1.Lease) {
			claim(a, 1)(l)
			l.Annotations["racer.unbounded-cloud.io/holder-node"] = "node-b"
		}))
		require.NoError(t, update(t, f, a.client, seat, claim(a, 1)))

		// Renewal keeps the term; only the holder publishes.
		require.NoError(t, update(t, f, a.client, seat, func(l *coordinationv1.Lease) { l.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()} }))
		denied(t, update(t, f, a.client, seat, func(l *coordinationv1.Lease) { l.Spec.LeaseTransitions = ptr.To(int32(5)) }))
		denied(t, update(t, f, b.client, seat, func(l *coordinationv1.Lease) {
			l.Annotations["racer.unbounded-cloud.io/endpoint"] = "https://10.0.0.2:8443"
		}))
		denied(t, update(t, f, b.client, seat, func(l *coordinationv1.Lease) { l.Spec.HolderIdentity = nil }))
		denied(t, update(t, f, a.client, seat, func(l *coordinationv1.Lease) { l.Labels["racer.unbounded-cloud.io/control"] = "false" }))
		denied(t, update(t, f, a.client, seat, func(l *coordinationv1.Lease) { l.Annotations["other"] = "x" }))

		// Admission has no clock: takeover is allowed as the caller with a new
		// term, and the runtime decides expiry.
		require.NoError(t, update(t, f, b.client, seat, claim(b, 2)))
		denied(t, update(t, f, a.client, seat, func(l *coordinationv1.Lease) {
			l.Spec.HolderIdentity = nil
		}))
		require.NoError(t, update(t, f, b.client, seat, func(l *coordinationv1.Lease) {
			l.Spec.HolderIdentity = nil
			delete(l.Annotations, "racer.unbounded-cloud.io/endpoint")
		}))

		// Nobody may publish on an unheld Lease, but stale annotations may go.
		denied(t, update(t, f, a.client, seat, func(l *coordinationv1.Lease) { l.Annotations["racer.unbounded-cloud.io/endpoint"] = "https://x" }))
		require.NoError(t, update(t, f, a.client, seat, func(l *coordinationv1.Lease) { delete(l.Annotations, "racer.unbounded-cloud.io/nonce") }))

		// RBAC: named Leases only; no create, delete, or namespace-wide list.
		require.NoError(t, a.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "racer-primary"}, &coordinationv1.Lease{}))
		denied(t, a.client.List(ctx, &coordinationv1.LeaseList{}, client.InNamespace(ns)))
		require.NoError(t, a.client.List(ctx, &coordinationv1.LeaseList{}, client.InNamespace(ns), client.MatchingFields{"metadata.name": "racer-seat-15"}))
		denied(t, a.client.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "racer-seat-16", Namespace: ns}}))
		denied(t, a.client.Delete(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "racer-seat-1", Namespace: ns}}))
	})

	t.Run("state", func(t *testing.T) {
		state := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: stateName, Namespace: ns}}
		initialize := func(cluster string) func(*corev1.Secret) {
			return func(s *corev1.Secret) {
				if cluster == "" {
					cluster = string(s.UID)
				}

				s.Annotations = map[string]string{"racer.unbounded-cloud.io/initialized": "v1", "racer.unbounded-cloud.io/cluster-id": cluster}
				s.Data = map[string][]byte{"cluster": []byte(cluster), "state": []byte(`{"term":1}`)}
			}
		}

		denied(t, update(t, f, a.client, state, initialize("other-cluster")))
		denied(t, update(t, f, a.client, state, func(s *corev1.Secret) {
			initialize("")(s)
			s.Data["extra"] = []byte("x")
		}))
		require.NoError(t, update(t, f, a.client, state, initialize("")))

		require.NoError(t, update(t, f, a.client, state, func(s *corev1.Secret) { s.Data["state"] = []byte(`{"term":2}`) }))
		denied(t, update(t, f, a.client, state, func(s *corev1.Secret) { s.Data = nil }))
		denied(t, update(t, f, a.client, state, func(s *corev1.Secret) { delete(s.Data, "state") }))
		denied(t, update(t, f, a.client, state, func(s *corev1.Secret) { s.Data["cluster"] = []byte("other") }))
		denied(t, update(t, f, a.client, state, func(s *corev1.Secret) { s.Annotations["racer.unbounded-cloud.io/cluster-id"] = "other" }))
		denied(t, update(t, f, a.client, state, func(s *corev1.Secret) { delete(s.Annotations, "racer.unbounded-cloud.io/initialized") }))
		denied(t, update(t, f, a.client, state, func(s *corev1.Secret) { s.Labels = nil }))

		denied(t, a.client.Delete(ctx, state))
		denied(t, a.client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "racer-other", Namespace: ns}}))
		denied(t, a.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "default-token"}, &corev1.Secret{}))
		denied(t, a.client.List(ctx, &corev1.SecretList{}, client.InNamespace(ns)))
	})

	t.Run("webhook configuration", func(t *testing.T) {
		config := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookConfigName}}
		require.NoError(t, patch(t, f, a.client, config, func(c *admissionv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].ClientConfig.CABundle = []byte("-----BEGIN CERTIFICATE-----")
		}))
		denied(t, patch(t, f, a.client, config, func(c *admissionv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].ClientConfig.Service.Name = "elsewhere"
		}))
		denied(t, patch(t, f, a.client, config, func(c *admissionv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].FailurePolicy = ptr.To(admissionv1.Ignore)
		}))
		denied(t, patch(t, f, a.client, config, func(c *admissionv1.MutatingWebhookConfiguration) { c.Labels = map[string]string{"x": "y"} }))
		denied(t, a.client.Delete(ctx, config))
		denied(t, a.client.List(ctx, &admissionv1.MutatingWebhookConfigurationList{}))
	})

	t.Run("webhook endpoints", func(t *testing.T) {
		slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: webhookName, Namespace: ns}}
		endpoint := func(name, namespace string) discoveryv1.Endpoint {
			return discoveryv1.Endpoint{
				Addresses: []string{"10.0.0.1"},
				TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: name, Namespace: namespace, UID: types.UID("uid-" + name)},
			}
		}

		require.NoError(t, update(t, f, a.client, slice, func(s *discoveryv1.EndpointSlice) {
			s.Endpoints = []discoveryv1.Endpoint{endpoint(a.pod.Name, ns), endpoint(b.pod.Name, ns)}
		}))
		denied(t, update(t, f, a.client, slice, func(s *discoveryv1.EndpointSlice) { s.Endpoints = []discoveryv1.Endpoint{endpoint("api", ns)} }))
		denied(t, update(t, f, a.client, slice, func(s *discoveryv1.EndpointSlice) {
			s.Endpoints = []discoveryv1.Endpoint{endpoint(a.pod.Name, "default")}
		}))
		denied(t, update(t, f, a.client, slice, func(s *discoveryv1.EndpointSlice) { s.Ports[0].Port = ptr.To(int32(443)) }))
		denied(t, update(t, f, a.client, slice, func(s *discoveryv1.EndpointSlice) { s.Labels[discoveryv1.LabelServiceName] = "kubernetes" }))
		denied(t, a.client.Create(ctx, &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: ns}, AddressType: discoveryv1.AddressTypeIPv4}))
	})

	t.Run("read only grants", func(t *testing.T) {
		require.NoError(t, a.client.List(ctx, &corev1.NodeList{}))
		denied(t, a.client.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-x"}}))
		require.NoError(t, a.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: dataplaneName}, &corev1.ServiceAccount{}))
		denied(t, a.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: "default"}, &corev1.ServiceAccount{}))
		denied(t, a.client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: ns}}))
		denied(t, a.client.List(ctx, &corev1.PodList{}, client.InNamespace("default")))
	})
}

// TestEnvtestOperatorPreservesRuntimeState installs through the real executor,
// lets the runtime publish trust and state, and checks that later operator
// passes keep them.
func TestEnvtestOperatorPreservesRuntimeState(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run the Racer operator tests against a real API server")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../../../api/racer/v1alpha1/crd"}, ErrorIfCRDPathMissing: true}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	s := scheme.Scheme
	require.NoError(t, racerv1.AddToScheme(s))

	admin, err := client.New(rc, client.Options{Scheme: s})
	require.NoError(t, err)

	ctx := t.Context()
	env := &component.Env{Client: admin, APIReader: admin, Scheme: s, Namespace: "custom-system", Config: component.Config{ImageRegistry: "mirror.test/unbounded", ImageTag: "v1"}}
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.Namespace}}))
	require.NoError(t, admin.Create(ctx, &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache"}}))

	converge := func() component.Result {
		var result component.Result

		for range 5 {
			var plan *component.Plan

			plan, result, err = (Component{}).Plan(ctx, env, nil)
			require.NoError(t, err)

			executed, err := env.Execute(ctx, plan)
			require.NoError(t, err)

			for _, op := range executed.Results {
				require.Equal(t, component.OpSucceeded, op.Status, "%s: %v", op.Ref, op.Err)
			}

			if result.Reason != reasonInitializing {
				break
			}
		}

		return result
	}

	require.NotEqual(t, reasonInitializing, converge().Reason)

	state := &corev1.Secret{}
	require.NoError(t, admin.Get(ctx, objectKey(env, stateName), state))
	state.Annotations = map[string]string{"racer.unbounded-cloud.io/initialized": "v1", "racer.unbounded-cloud.io/cluster-id": string(state.UID)}
	state.Data = map[string][]byte{"cluster": []byte(state.UID), "state": []byte("{}")}
	require.NoError(t, admin.Update(ctx, state, client.FieldOwner("racer-dataplane")))

	webhook := &admissionv1.MutatingWebhookConfiguration{}
	require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: webhookConfigName}, webhook))
	base := webhook.DeepCopy()
	webhook.Webhooks[0].ClientConfig.CABundle = []byte("runtime-ca")
	require.NoError(t, admin.Patch(ctx, webhook, client.MergeFrom(base), client.FieldOwner("racer-dataplane")))

	slice := &discoveryv1.EndpointSlice{}
	require.NoError(t, admin.Get(ctx, objectKey(env, webhookName), slice))
	slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}}}
	require.NoError(t, admin.Update(ctx, slice, client.FieldOwner("racer-dataplane")))

	// Drift the webhook so the operator really reapplies it.
	require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: webhookConfigName}, webhook))
	base = webhook.DeepCopy()
	webhook.Webhooks[0].TimeoutSeconds = ptr.To(int32(9))
	require.NoError(t, admin.Patch(ctx, webhook, client.MergeFrom(base), client.FieldOwner("someone")))

	converge()

	require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: webhookConfigName}, webhook))
	require.Equal(t, []byte("runtime-ca"), webhook.Webhooks[0].ClientConfig.CABundle)
	require.Equal(t, int32(5), *webhook.Webhooks[0].TimeoutSeconds)

	after := &corev1.Secret{}
	require.NoError(t, admin.Get(ctx, objectKey(env, stateName), after))
	require.Equal(t, state.UID, after.UID)
	require.Equal(t, state.Data, after.Data)
	require.Equal(t, "v1", after.Annotations["racer.unbounded-cloud.io/initialized"])

	require.NoError(t, admin.Get(ctx, objectKey(env, webhookName), slice))
	require.Len(t, slice.Endpoints, 1)
}
