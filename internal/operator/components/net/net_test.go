// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

func newNetTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		appsv1.AddToScheme,
		corev1.AddToScheme,
		discoveryv1.AddToScheme,
		admissionregistrationv1.AddToScheme,
		apiregistrationv1.AddToScheme,
		unboundedv1alpha3.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return scheme
}

func testEnv(t *testing.T, objects ...client.Object) *component.Env {
	t.Helper()

	return &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(newNetTestScheme(t)).WithObjects(objects...).Build(),
		Namespace: component.DefaultNamespace,
	}
}

// testCA is the serving CA fixture. Its exact bytes matter only in that
// stamping must reproduce them base64-encoded.
var testCA = []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----")

// servingBackend is the state of a controller that is rolled out and answering.
func servingBackend() backendState {
	return backendState{caBundle: testCA, ready: true}
}

// servingObjects is the cluster state readBackendState reads as serving: a
// published CA, a Deployment whose current spec has rolled out, and an
// endpoint behind the Service.
func servingObjects() []client.Object {
	deploymentUID := types.UID("controller-deployment-uid")
	replicaSetUID := types.UID("controller-replicaset-uid")
	podUID := types.UID("controller-pod-uid")
	portName := "https"
	port := int32(9999)
	protocol := corev1.ProtocolTCP
	ready := true

	return []client.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: servingCAName},
			Data:       map[string]string{servingCAKey: string(testCA)},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:  component.DefaultNamespace,
				Name:       controllerName,
				Generation: 3,
				UID:        deploymentUID,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": controllerName}},
			},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 3,
				Replicas:           1,
				UpdatedReplicas:    1,
				ReadyReplicas:      1,
				AvailableReplicas:  1,
			},
		},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: component.DefaultNamespace,
				Name:      controllerName,
				Labels:    map[string]string{discoveryv1.LabelServiceName: controllerName},
			},
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
				TargetRef: &corev1.ObjectReference{
					Kind: "Pod", Namespace: component.DefaultNamespace, Name: "controller-pod", UID: podUID,
				},
			}},
			Ports: []discoveryv1.EndpointPort{{Name: &portName, Port: &port, Protocol: &protocol}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: component.DefaultNamespace,
				Name:      "controller-pod",
				UID:       podUID,
				Labels:    map[string]string{"app": controllerName},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "controller-rs", UID: replicaSetUID, Controller: ptr.To(true),
				}},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.1",
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionTrue,
				}},
			},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: component.DefaultNamespace,
				Name:      "controller-rs",
				UID:       replicaSetUID,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "Deployment", Name: controllerName, UID: deploymentUID, Controller: ptr.To(true),
				}},
			},
		},
	}
}

func TestEnsureConfigCreatesDefaultOnlyWhenAbsent(t *testing.T) {
	env := testEnv(t)

	hash, err := ensureConfig(t, env)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap

	key := client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}
	if err := env.Client.Get(t.Context(), key, &got); err != nil {
		t.Fatalf("get default net config: %v", err)
	}

	if got.Data["config.yaml"] == "" || hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("default net config/hash missing: hash=%q data=%#v", hash, got.Data)
	}

	if !strings.Contains(got.Data["config.yaml"], "stunEnabled: false") {
		t.Fatal("default net config must explicitly disable STUN")
	}

	if !strings.Contains(got.Data["config.yaml"], "stunHost: \"\"\n  stunPort: 3478") {
		t.Fatal("default net config must not select a STUN endpoint")
	}

	if !strings.Contains(got.Data["config.yaml"], "stunRecheckInterval: \"1h\"") {
		t.Fatal("default net config must expose the STUN recheck interval")
	}

	if strings.Contains(got.Data["config.yaml"], "stunTimeout:") {
		t.Fatal("default net config must not expose the STUN request timeout")
	}
}

func TestEnsureConfigPreservesExistingPayload(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace},
		Data:       map[string]string{"config.yaml": "custom: true", "extra": "keep"},
		BinaryData: map[string][]byte{"routing.bin": {0, 1, 2}},
	}
	env := testEnv(t, existing)

	hash, err := ensureConfig(t, env)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get preserved net config: %v", err)
	}

	if got.Data["config.yaml"] != "custom: true" || got.Data["extra"] != "keep" {
		t.Fatalf("existing net config changed: %#v", got.Data)
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact payload hash", hash)
	}
}

func TestApplyMutatorStampsBothWorkloads(t *testing.T) {
	cfg := component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"}

	for _, tc := range []struct{ kind, name string }{
		{kind: "Deployment", name: controllerName},
		{kind: "DaemonSet", name: nodeName},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       tc.kind,
				"metadata":   map[string]any{"name": tc.name},
				"spec": map[string]any{"template": map[string]any{
					"metadata": map[string]any{"annotations": map[string]any{"existing": "kept"}},
					"spec": map[string]any{
						"initContainers": []any{map[string]any{"name": "init", "image": "old:init"}},
						"containers":     []any{map[string]any{"name": "main", "image": "old:main"}},
					},
				}},
			}}

			if err := applyMutator(cfg, "net-hash", servingBackend())(obj); err != nil {
				t.Fatalf("applyMutator: %v", err)
			}

			annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
			if annotations[ConfigHashAnnotation] != "net-hash" || annotations["existing"] != "kept" {
				t.Fatalf("pod template annotations = %#v", annotations)
			}

			wantRepository := "unbounded-net-controller"
			if tc.name == nodeName {
				wantRepository = "unbounded-net-node"
			}

			for _, field := range []string{"initContainers", "containers"} {
				containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
				if got := containers[0].(map[string]any)["image"]; got != "registry.example.com/"+wantRepository+":v1.2.3" {
					t.Fatalf("%s image = %q", field, got)
				}
			}
		})
	}

	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": configName},
	}}
	if err := applyMutator(cfg, "net-hash", servingBackend())(config); err != nil || config.Object != nil {
		t.Fatalf("embedded net ConfigMap was not skipped: err=%v object=%#v", err, config.Object)
	}

	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": component.CRDKind,
		"metadata": map[string]any{"name": "sites.unbounded-cloud.io"},
	}}
	if err := applyMutator(cfg, "net-hash", servingBackend())(crd); err != nil || crd.Object != nil {
		t.Fatalf("CRD was not skipped: err=%v object=%#v", err, crd.Object)
	}
}

func TestReconcileRetainedWithNoSites(t *testing.T) {
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: configName},
		Data:       map[string]string{"config.yaml": "custom: retained"},
	}
	// Serving, so this stays a test about retaining the config payload rather
	// than about the registration gate.
	env, appliedHashes := retainedEnv(t, append(servingObjects(), config)...)

	res := reconcile(t, env, nil)
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	wantHash := component.ConfigMapPayloadHash(config)
	for _, name := range []string{controllerName, nodeName} {
		if appliedHashes[name] != wantHash {
			t.Fatalf("%s applied hash = %q, want %q", name, appliedHashes[name], wantHash)
		}
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(config), &got); err != nil {
		t.Fatalf("get retained net config: %v", err)
	}

	if got.Data["config.yaml"] != "custom: retained" {
		t.Fatalf("retained net config changed: %#v", got.Data)
	}
}

func TestReconcileRecreatesDeletedRetainedConfigWithNoSites(t *testing.T) {
	// servingObjects carries the retained controller Deployment, which is what
	// makes this a retained installation, and makes it serving so the assertion
	// stays about the recreated config.
	env, appliedHashes := retainedEnv(t, servingObjects()...)

	res := reconcile(t, env, nil)
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	var config corev1.ConfigMap

	key := client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}
	if err := env.Client.Get(t.Context(), key, &config); err != nil {
		t.Fatalf("get recreated net config: %v", err)
	}

	wantHash := component.ConfigMapPayloadHash(&config)
	if config.Data["config.yaml"] == "" || appliedHashes[controllerName] != wantHash || appliedHashes[nodeName] != wantHash {
		t.Fatalf("recreated config/workload hashes = data=%#v hashes=%#v", config.Data, appliedHashes)
	}
}

func TestReconcileDoesNotCreateFromNothingWithNoSites(t *testing.T) {
	env, appliedHashes := retainedEnv(t)

	res := reconcile(t, env, nil)
	if !res.Ready || res.Reason != component.ReasonNoSites {
		t.Fatalf("Reconcile = %+v, want ready with NoSites", res)
	}

	err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) || len(appliedHashes) != 0 {
		t.Fatalf("zero-Site reconcile created net from nothing: config err=%v hashes=%#v", err, appliedHashes)
	}
}

func retainedEnv(t *testing.T, objects ...client.Object) (*component.Env, map[string]string) {
	t.Helper()

	scheme := newNetTestScheme(t)

	appliedHashes := map[string]string{}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applied, ok := obj.(interface {
					GetName() string
					GetKind() string
					UnstructuredContent() map[string]any
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				name := applied.GetName()
				if (applied.GetKind() != "Deployment" || name != controllerName) &&
					(applied.GetKind() != "DaemonSet" || name != nodeName) {
					return nil
				}

				hash, _, err := unstructured.NestedString(
					applied.UnstructuredContent(),
					"spec", "template", "metadata", "annotations", ConfigHashAnnotation,
				)
				if err != nil {
					t.Fatalf("get applied hash for %s: %v", name, err)
				}

				appliedHashes[name] = hash

				return nil
			},
		}).
		Build()

	return &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}, appliedHashes
}

// gateEnv records every object a reconcile applies, keyed by Kind/name, so a
// test can assert on what was withheld as well as on what was written.
func gateEnv(t *testing.T, objects ...client.Object) (*component.Env, map[string]*unstructured.Unstructured) {
	t.Helper()

	applied := map[string]*unstructured.Unstructured{}
	scheme := newNetTestScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				object, ok := obj.(interface {
					GetName() string
					GetKind() string
					UnstructuredContent() map[string]any
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				applied[object.GetKind()+"/"+object.GetName()] = &unstructured.Unstructured{
					Object: object.UnstructuredContent(),
				}

				return nil
			},
		}).
		Build()

	return &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}, applied
}

// ensureConfig plans and executes the net config operation, mirroring what the
// reconciler does so these tests exercise the production path.
func ensureConfig(t *testing.T, env *component.Env) (string, error) {
	t.Helper()

	hash, op, err := planConfig(t.Context(), env)
	if err != nil {
		return "", err
	}

	if op == nil {
		return hash, nil
	}

	plan := component.NewPlan()
	plan.Add(*op)

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		return "", err
	}

	return hash, exec.Err()
}

// reconcile plans and executes the component, mirroring the reconciler.
func reconcile(t *testing.T, env *component.Env, sites []unboundedv1alpha3.Site) component.Result {
	t.Helper()

	c := Component{}

	plan, res, err := c.Plan(t.Context(), env, sites)
	if err != nil {
		return component.Failed(err)
	}

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		return component.Failed(err)
	}

	// net is a cluster component and plans no per-Site operations, so there is
	// no Site to attribute results to.
	return component.CombineResult(c.Name(), "", res, exec)
}

// site is the one Site that makes net reconcile at all.
func site() []unboundedv1alpha3.Site {
	return []unboundedv1alpha3.Site{{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}}
}

// appliedRegistrations returns the recorded registrations, by name.
func appliedRegistrations(applied map[string]*unstructured.Unstructured) []string {
	var names []string

	for key, obj := range applied {
		if isBackendRegistration(obj) {
			names = append(names, key)
		}
	}

	slices.Sort(names)

	return names
}

// TestReconcileWithholdsRegistrationsUntilTheBackendServes is the point of the
// gate.
//
// A ValidatingWebhookConfiguration with failurePolicy: Ignore that points at a
// backend which is not listening does not fail loudly, it silently stops
// enforcing, and an APIService in the same state makes the aggregated API
// return errors for a type the cluster believes is served. Neither is
// detectable from the object itself, so the registration is not written until
// something is behind the Service.
func TestReconcileWithholdsRegistrationsUntilTheBackendServes(t *testing.T) {
	rolledOut := func(mutate func(*appsv1.Deployment)) client.Object {
		deployment, ok := servingObjects()[1].(*appsv1.Deployment)
		if !ok {
			t.Fatal("servingObjects[1] is not the controller Deployment")
		}

		mutate(deployment)

		return deployment
	}

	cases := []struct {
		name       string
		objects    []client.Object
		wantReason string
	}{
		{
			name:       "no serving CA published",
			objects:    nil,
			wantReason: "has not published its serving CA",
		},
		{
			name: "serving CA ConfigMap is empty",
			objects: []client.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: servingCAName},
			}},
			wantReason: "carries no " + servingCAKey,
		},
		{
			name:       "controller Deployment does not exist",
			objects:    servingObjects()[:1],
			wantReason: "Deployment does not exist yet",
		},
		{
			name: "controller Deployment is still rolling out",
			objects: []client.Object{
				servingObjects()[0],
				rolledOut(func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 0 }),
				servingObjects()[2],
			},
			wantReason: "is rolling out",
		},
		{
			name: "controller Deployment has not been observed",
			objects: []client.Object{
				servingObjects()[0],
				rolledOut(func(d *appsv1.Deployment) { d.Status.ObservedGeneration = 1 }),
				servingObjects()[2],
			},
			wantReason: "has not been observed at its current generation",
		},
		{
			name:       "no endpoint behind the Service",
			objects:    servingObjects()[:2],
			wantReason: "no endpoint is registered",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, applied := gateEnv(t, tc.objects...)

			res := reconcile(t, env, site())

			if res.Ready || res.Err != nil {
				t.Fatalf("Reconcile = %+v, want not ready without an error", res)
			}

			if res.Reason != component.ReasonBackendNotReady {
				t.Fatalf("reason = %q, want %q", res.Reason, component.ReasonBackendNotReady)
			}

			if !strings.Contains(res.Message, tc.wantReason) {
				t.Fatalf("message = %q, want it to mention %q", res.Message, tc.wantReason)
			}

			// Without a requeue the gate would wait for an event that never
			// comes: readiness lives in Deployment status and Endpoints, and
			// neither is watched.
			if res.RequeueAfter != backendPollInterval {
				t.Fatalf("RequeueAfter = %s, want %s", res.RequeueAfter, backendPollInterval)
			}

			if got := appliedRegistrations(applied); len(got) != 0 {
				t.Fatalf("registered against a backend that is not serving: %v", got)
			}

			// The workloads are still applied: withholding registration must
			// not withhold the thing that makes the backend come up.
			for _, key := range []string{"Deployment/" + controllerName, "DaemonSet/" + nodeName} {
				if applied[key] == nil {
					t.Fatalf("%s was not applied; the backend can never become ready", key)
				}
			}
		})
	}
}

// TestReconcileRegistersAndStampsWhenTheBackendServes covers the other half:
// once the backend serves, all three registrations go out carrying the CA.
func TestReconcileRegistersAndStampsWhenTheBackendServes(t *testing.T) {
	env, applied := gateEnv(t, servingObjects()...)

	res := reconcile(t, env, site())
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	got := appliedRegistrations(applied)
	if len(got) != len(registrations) {
		t.Fatalf("applied registrations = %v, want %d of them", got, len(registrations))
	}

	want := base64.StdEncoding.EncodeToString(testCA)

	for _, registration := range registrations {
		obj := applied[registration.gvk.Kind+"/"+registration.name]
		if obj == nil {
			t.Fatalf("%s %s was not applied", registration.gvk.Kind, registration.name)
		}

		if !hasCABundle(obj, nil) {
			t.Fatalf("%s went out with an empty caBundle; the apiserver cannot verify the backend", registration.name)
		}

		for _, bundle := range caBundlesOf(t, obj) {
			if bundle != want {
				t.Fatalf("%s caBundle = %q, want the published CA", registration.name, bundle)
			}
		}
	}
}

// TestPlanReadsBackendStateThroughTheAPIReader pins which reader the gate uses,
// since the whole point of Env.APIReader is that the two disagree.
//
// The cache is populated by a watch, so it lags the apiserver: just after a
// rollout starts it still describes the settled previous revision. A gate
// reading it would register against a backend that is on its way down, which
// is precisely the state a registration must not be created for.
func TestPlanReadsBackendStateThroughTheAPIReader(t *testing.T) {
	scheme := newNetTestScheme(t)

	// The cache still shows the previous revision, fully rolled out.
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(servingObjects()...).Build()

	rollingOut := servingObjects()

	deployment, ok := rollingOut[1].(*appsv1.Deployment)
	if !ok {
		t.Fatal("servingObjects[1] is not the controller Deployment")
	}

	// The apiserver has accepted a new pod template. Status still describes the
	// old revision until the Deployment controller observes it.
	deployment.Generation++

	live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollingOut...).Build()

	env := &component.Env{
		Client:    cached,
		APIReader: live,
		Scheme:    scheme,
		Namespace: component.DefaultNamespace,
	}

	plan, res, err := (Component{}).Plan(t.Context(), env, site())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if res.Ready || res.Reason != component.ReasonBackendNotReady {
		t.Fatalf("result = %+v, want the in-flight rollout to hold registrations back", res)
	}

	for _, registration := range registrations {
		if strings.Contains(plan.Summary(), registration.name) {
			t.Fatalf("%s was planned from cached readiness describing the outgoing revision", registration.name)
		}
	}
}

// TestReconcileStaysReadyWhenWithholdingChangesNothing pins the reporting rule.
//
// The controller is host-networked with maxSurge: 0, so it is briefly
// unavailable by design on every upgrade. Reporting a withheld registration
// that is already in place and usable would turn NetReady False on every net
// rollout, which is noise rather than signal.
func TestReconcileStaysReadyWhenWithholdingChangesNothing(t *testing.T) {
	existing := existingRegistrations(t, base64.StdEncoding.EncodeToString(testCA))

	// Serving CA published, but the Deployment is mid-rollout.
	objects := append(existing, servingObjects()[0])

	env, applied := gateEnv(t, objects...)

	res := reconcile(t, env, site())
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready: withholding a registration that is already usable is not a status change", res)
	}

	if got := appliedRegistrations(applied); len(got) != 0 {
		t.Fatalf("rewrote registrations while the backend was down: %v", got)
	}

	if res.RequeueAfter != backendIdlePollInterval {
		t.Fatalf("RequeueAfter = %s, want the idle interval %s: with nothing pending there is no drift to converge, "+
			"and the fast interval re-applies net's whole manifest set once per Site per tick", res.RequeueAfter, backendIdlePollInterval)
	}
}

// TestReconcileReportsARegistrationWithAnEmptyCABundle is the case the no-flap
// rule must not swallow: a registration that exists but carries no CA is the
// broken state this gate prevents, and it stays visible until it can be fixed.
func TestReconcileReportsARegistrationWithAnEmptyCABundle(t *testing.T) {
	existing := existingRegistrations(t, "")

	env, _ := gateEnv(t, append(existing, servingObjects()[0])...)

	res := reconcile(t, env, site())
	if res.Ready {
		t.Fatal("a registration with an empty caBundle was reported as ready")
	}

	if res.Reason != component.ReasonBackendNotReady {
		t.Fatalf("reason = %q, want %q", res.Reason, component.ReasonBackendNotReady)
	}
}

func TestReconcileReportsRegistrationsWithAStaleCABundle(t *testing.T) {
	oldCA := base64.StdEncoding.EncodeToString([]byte("old CA"))
	existing := existingRegistrations(t, oldCA)

	// The current CA is published, but the controller is still rolling out.
	// Existing registrations trust a different CA and are not usable.
	env, _ := gateEnv(t, append(existing, servingObjects()[0])...)

	res := reconcile(t, env, site())
	if res.Ready || res.Reason != component.ReasonBackendNotReady {
		t.Fatalf("Reconcile = %+v, want stale registration CAs reported as pending", res)
	}
}

// TestReconcileKeepsConvergingWhenTheBackendReadFails pins the blast radius of
// a failed readiness read.
//
// The gate reads endpoints, pods and ReplicaSets: objects the operator does not
// own, over permissions granted separately from the ones it applies with. A 403
// during an upgrade whose ClusterRole has not landed yet is therefore a real
// state, and it must not stop net reconciling. Failing the plan would withhold
// the ConfigMap, the Deployment and the node DaemonSet as well, leaving the
// dataplane unable to converge over a question that governs three
// registrations. Withholding is the safe direction on an unknown; writing
// nothing is not.
func TestReconcileKeepsConvergingWhenTheBackendReadFails(t *testing.T) {
	readFailure := errors.New("endpointslices.discovery.k8s.io is forbidden")

	scheme := newNetTestScheme(t)
	applied := map[string]*unstructured.Unstructured{}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(servingObjects()[:2]...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*discoveryv1.EndpointSlice); ok {
					return readFailure
				}

				return c.Get(ctx, key, obj, opts...)
			},
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				object, ok := obj.(interface {
					GetName() string
					GetKind() string
					UnstructuredContent() map[string]any
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				applied[object.GetKind()+"/"+object.GetName()] = &unstructured.Unstructured{
					Object: object.UnstructuredContent(),
				}

				return nil
			},
		}).
		Build()

	env := &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}

	res := reconcile(t, env, site())

	// The workloads are the point: they must still be written.
	for _, want := range []string{
		"Deployment/" + controllerName,
		"DaemonSet/" + nodeName,
		"Service/" + controllerName,
	} {
		if applied[want] == nil {
			t.Fatalf("%s was not applied; a failed readiness read stopped net converging", want)
		}
	}

	if got := appliedRegistrations(applied); len(got) != 0 {
		t.Fatalf("registered against a backend whose state could not be read: %v", got)
	}

	if res.Ready || res.Reason != component.ReasonBackendNotReady {
		t.Fatalf("result = %+v, want not ready with %q", res, component.ReasonBackendNotReady)
	}

	// The error is still surfaced, so the pass goes through error backoff and
	// the failure is not mistaken for an orderly wait.
	if !errors.Is(res.Err, readFailure) {
		t.Fatalf("result Err = %v, want it to carry the read failure", res.Err)
	}
}

// TestReconcileDoesNotGateAdmissionPolicies guards the scope of the gate.
//
// ValidatingAdmissionPolicies are evaluated inside the apiserver with no
// backend to reach, so withholding them while the controller is down would
// drop enforcement for no reason at all.
func TestReconcileDoesNotGateAdmissionPolicies(t *testing.T) {
	env, applied := gateEnv(t)

	if res := reconcile(t, env, site()); res.Ready {
		t.Fatalf("Reconcile = %+v, want not ready with no backend", res)
	}

	var policies int

	for key := range applied {
		if strings.HasPrefix(key, "ValidatingAdmissionPolicy") {
			policies++
		}
	}

	if policies == 0 {
		t.Fatal("admission policies were withheld along with the registrations; they have no backend to wait for")
	}
}

// TestRegistrationIdentitiesMatchTheManifests keeps the gate's hardcoded
// identities honest. The gate looks registrations up in the cluster before it
// applies anything, so it cannot learn their names from the manifests it is
// about to apply; this fails if a manifest is renamed out from under it.
func TestRegistrationIdentitiesMatchTheManifests(t *testing.T) {
	env, applied := gateEnv(t, servingObjects()...)

	if res := reconcile(t, env, site()); !res.Ready {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	for key, obj := range applied {
		if !isBackendRegistration(obj) {
			continue
		}

		if !slices.ContainsFunc(registrations, func(r struct {
			gvk  schema.GroupVersionKind
			name string
		},
		) bool {
			return r.gvk.Kind == obj.GetKind() && r.name == obj.GetName()
		}) {
			t.Fatalf("manifest ships registration %s which the gate does not know about; it would never be withheld", key)
		}
	}

	if len(appliedRegistrations(applied)) != len(registrations) {
		t.Fatalf("the gate knows about %d registrations but the manifests ship %d",
			len(registrations), len(appliedRegistrations(applied)))
	}
}

func TestRolloutComplete(t *testing.T) {
	cases := []struct {
		name       string
		deployment appsv1.Deployment
		wantDone   bool
		wantReason string
	}{
		{
			name: "rolled out",
			deployment: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
				},
			},
			wantDone: true,
		},
		{
			// A Deployment scaled to zero is never going to serve, and its
			// counters all read as satisfied, so it has to be caught first.
			name: "scaled to zero",
			deployment: appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(0))},
				Status: appsv1.DeploymentStatus{ObservedGeneration: 1},
			},
			wantReason: "scaled to zero",
		},
		{
			name: "status is stale",
			deployment: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 5},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 4, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
				},
			},
			wantReason: "has not been observed",
		},
		{
			// An old replica still being available is the case the gate must
			// not accept: the CA and Service reference being registered belong
			// to the new spec.
			name: "old replica still serving",
			deployment: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 0, ReadyReplicas: 1, AvailableReplicas: 1,
				},
			},
			wantReason: "is rolling out",
		},
		{
			name: "updated but not available",
			deployment: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 0, AvailableReplicas: 0,
				},
			},
			wantReason: "is rolling out",
		},
		{
			name: "nil replicas defaults to one",
			deployment: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 1},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
				},
			},
			wantDone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, done := rolloutComplete(&tc.deployment)

			if done != tc.wantDone {
				t.Fatalf("rolloutComplete = %v (%q), want %v", done, reason, tc.wantDone)
			}

			if !tc.wantDone && !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.wantReason)
			}
		})
	}
}

// TestServiceHasEndpointFallsBackToEndpoints covers the legacy path. The
// controller writes both objects, and an apiserver old enough to resolve an
// APIService through Endpoints alone is why it still does.
func TestServiceHasEndpointFallsBackToEndpoints(t *testing.T) {
	objects := servingObjects()
	deployment := objects[1].(*appsv1.Deployment)
	pod := objects[3]
	replicaSet := objects[4]
	target := corev1.ObjectReference{
		Kind: "Pod", Namespace: component.DefaultNamespace, Name: "controller-pod", UID: types.UID("controller-pod-uid"),
	}
	notReady := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: component.DefaultNamespace,
			Name:      controllerName,
			Labels:    map[string]string{discoveryv1.LabelServiceName: controllerName},
		},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.9"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)},
			TargetRef:  &target,
		}},
		Ports: []discoveryv1.EndpointPort{{Name: ptr.To("https"), Port: ptr.To(int32(9999)), Protocol: ptr.To(corev1.ProtocolTCP)}},
	}

	endpoints := &corev1.Endpoints{ //nolint:staticcheck // exercising the legacy path on purpose
		ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: controllerName},
		Subsets: []corev1.EndpointSubset{{ //nolint:staticcheck // exercising the legacy path on purpose
			Addresses: []corev1.EndpointAddress{{IP: "10.0.0.1", TargetRef: &target}},
			Ports:     []corev1.EndpointPort{{Name: "https", Port: 9999, Protocol: corev1.ProtocolTCP}},
		}},
	}

	// Legacy Endpoints are used only when the authoritative EndpointSlice is
	// absent.
	env := testEnv(t, endpoints, pod, replicaSet)

	serving, err := serviceHasEndpoint(t.Context(), env.LiveReader(), env.Namespace, deployment)
	if err != nil {
		t.Fatalf("serviceHasEndpoint: %v", err)
	}

	if !serving {
		t.Fatal("a ready legacy Endpoints subset was not recognized as serving")
	}

	// An existing not-ready slice is authoritative even if a legacy object has
	// a ready address.
	bare := testEnv(t, notReady, endpoints, pod, replicaSet)

	serving, err = serviceHasEndpoint(t.Context(), bare.LiveReader(), bare.Namespace, deployment)
	if err != nil {
		t.Fatalf("serviceHasEndpoint: %v", err)
	}

	if serving {
		t.Fatal("legacy Endpoints overrode an authoritative not-ready EndpointSlice")
	}
}

func TestServiceHasEndpointRejectsStaleOrMalformedTargets(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*discoveryv1.EndpointSlice, *corev1.Pod, *appsv1.ReplicaSet)
	}{
		{
			name: "wrong target UID",
			mutate: func(slice *discoveryv1.EndpointSlice, _ *corev1.Pod, _ *appsv1.ReplicaSet) {
				slice.Endpoints[0].TargetRef.UID = types.UID("old-pod")
			},
		},
		{
			// The port number is deliberately not matched, so the name is the
			// only thing that identifies the serving port. See hasHTTPSPort.
			name: "wrong port name",
			mutate: func(slice *discoveryv1.EndpointSlice, _ *corev1.Pod, _ *appsv1.ReplicaSet) {
				*slice.Ports[0].Name = "metrics"
			},
		},
		{
			name: "pod not ready",
			mutate: func(_ *discoveryv1.EndpointSlice, pod *corev1.Pod, _ *appsv1.ReplicaSet) {
				pod.Status.Conditions[0].Status = corev1.ConditionFalse
			},
		},
		{
			name: "pod owned by another deployment",
			mutate: func(_ *discoveryv1.EndpointSlice, _ *corev1.Pod, replicaSet *appsv1.ReplicaSet) {
				replicaSet.OwnerReferences[0].UID = types.UID("other-deployment")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := servingObjects()
			deployment := objects[1].(*appsv1.Deployment)
			slice := objects[2].(*discoveryv1.EndpointSlice)
			pod := objects[3].(*corev1.Pod)
			replicaSet := objects[4].(*appsv1.ReplicaSet)
			tc.mutate(slice, pod, replicaSet)

			env := testEnv(t, slice, pod, replicaSet)

			serving, err := serviceHasEndpoint(t.Context(), env.LiveReader(), env.Namespace, deployment)
			if err != nil {
				t.Fatalf("serviceHasEndpoint: %v", err)
			}

			if serving {
				t.Fatal("stale or malformed endpoint was accepted as serving")
			}
		})
	}
}

// TestServiceHasEndpointAcceptsAnEndpointWithoutATargetRef pins the
// compatibility path for controllers released before this gate existed.
//
// Those controllers publish their Endpoints and EndpointSlice with no
// targetRef, and a workload override may pin the controller image to such a
// version indefinitely. Rejecting them would freeze the registrations at
// whatever the cluster already had, silently: nothing would be pending, so
// NetReady would stay true while manifest changes were never applied.
func TestServiceHasEndpointAcceptsAnEndpointWithoutATargetRef(t *testing.T) {
	objects := servingObjects()

	deployment, ok := objects[1].(*appsv1.Deployment)
	if !ok {
		t.Fatal("servingObjects[1] is not the controller Deployment")
	}

	slice, ok := objects[2].(*discoveryv1.EndpointSlice)
	if !ok {
		t.Fatal("servingObjects[2] is not the controller EndpointSlice")
	}

	slice.Endpoints[0].TargetRef = nil

	// Neither the Pod nor the ReplicaSet is loaded: an endpoint with no
	// targetRef cannot be traced to one, and the rollout check the caller has
	// already passed is what establishes that a Ready pod exists.
	env := testEnv(t, slice)

	serving, err := serviceHasEndpoint(t.Context(), env.LiveReader(), env.Namespace, deployment)
	if err != nil {
		t.Fatalf("serviceHasEndpoint: %v", err)
	}

	if !serving {
		t.Fatal("an endpoint published without a targetRef was rejected; a pinned older controller would never register")
	}

	// An address list that is empty is still not a backend.
	slice.Endpoints[0].Addresses = nil

	env = testEnv(t, slice)

	serving, err = serviceHasEndpoint(t.Context(), env.LiveReader(), env.Namespace, deployment)
	if err != nil {
		t.Fatalf("serviceHasEndpoint: %v", err)
	}

	if serving {
		t.Fatal("an endpoint with no targetRef and no address was accepted as serving")
	}
}

// TestServiceHasEndpointIgnoresThePortNumber pins the port match to the name.
//
// The controller publishes controller.healthPort from its own config, so a
// number here would turn any healthPort override into a backend that never
// reads as serving, withholding the registrations permanently. A nil port is
// accepted for the same reason: the EndpointSlice API defines it as
// unrestricted, and the number is not a value this gate compares.
func TestServiceHasEndpointIgnoresThePortNumber(t *testing.T) {
	cases := []struct {
		name string
		port *int32
	}{
		{name: "non-default port", port: ptr.To(int32(8443))},
		{name: "unrestricted port", port: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := servingObjects()

			deployment, ok := objects[1].(*appsv1.Deployment)
			if !ok {
				t.Fatal("servingObjects[1] is not the controller Deployment")
			}

			slice, ok := objects[2].(*discoveryv1.EndpointSlice)
			if !ok {
				t.Fatal("servingObjects[2] is not the controller EndpointSlice")
			}

			slice.Ports[0].Port = tc.port

			env := testEnv(t, slice, objects[3], objects[4])

			serving, err := serviceHasEndpoint(t.Context(), env.LiveReader(), env.Namespace, deployment)
			if err != nil {
				t.Fatalf("serviceHasEndpoint: %v", err)
			}

			if !serving {
				t.Fatal("a serving backend was rejected over its port number")
			}
		})
	}
}

func TestStampCABundle(t *testing.T) {
	want := base64.StdEncoding.EncodeToString(testCA)

	apiService := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiregistration.k8s.io/v1", "kind": "APIService",
		"metadata": map[string]any{"name": "v1alpha1.status.net.unbounded-cloud.io"},
		"spec":     map[string]any{"group": "status.net.unbounded-cloud.io"},
	}}

	if err := stampCABundle(apiService, testCA); err != nil {
		t.Fatalf("stampCABundle: %v", err)
	}

	if got, _, _ := unstructured.NestedString(apiService.Object, "spec", "caBundle"); got != want {
		t.Fatalf("APIService caBundle = %q, want %q", got, want)
	}

	webhook := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration",
		"metadata": map[string]any{"name": "unbounded-net-validating-webhook"},
		"webhooks": []any{
			map[string]any{"name": "a.example.com", "clientConfig": map[string]any{"service": map[string]any{"name": "svc"}}},
			map[string]any{"name": "b.example.com", "clientConfig": map[string]any{"service": map[string]any{"name": "svc"}}},
		},
	}}

	if err := stampCABundle(webhook, testCA); err != nil {
		t.Fatalf("stampCABundle: %v", err)
	}

	// Every webhook in the configuration needs it, not just the first.
	for _, bundle := range caBundlesOf(t, webhook) {
		if bundle != want {
			t.Fatalf("webhook caBundle = %q, want %q", bundle, want)
		}
	}

	// A registration that declares no webhooks is a manifest bug, and silently
	// stamping nothing would hide it.
	empty := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingWebhookConfiguration",
		"metadata": map[string]any{"name": "broken"},
	}}
	if err := stampCABundle(empty, testCA); err == nil {
		t.Fatal("stamping a configuration with no webhooks must be an error")
	}
}

func TestHasCABundle(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{
			name: "APIService with a bundle",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"kind": "APIService", "spec": map[string]any{"caBundle": "abc"},
			}},
			want: true,
		},
		{
			name: "APIService without one",
			obj:  &unstructured.Unstructured{Object: map[string]any{"kind": "APIService", "spec": map[string]any{}}},
		},
		{
			name: "webhook configuration with every bundle set",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"kind": "ValidatingWebhookConfiguration",
				"webhooks": []any{
					map[string]any{"clientConfig": map[string]any{"caBundle": "abc"}},
					map[string]any{"clientConfig": map[string]any{"caBundle": "abc"}},
				},
			}},
			want: true,
		},
		{
			// One unusable webhook makes the configuration unusable, so the
			// emptiest bundle decides.
			name: "webhook configuration with one bundle missing",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"kind": "ValidatingWebhookConfiguration",
				"webhooks": []any{
					map[string]any{"clientConfig": map[string]any{"caBundle": "abc"}},
					map[string]any{"clientConfig": map[string]any{}},
				},
			}},
		},
		{
			name: "webhook configuration with no webhooks",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"kind": "ValidatingWebhookConfiguration",
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasCABundle(tc.obj, nil); got != tc.want {
				t.Fatalf("hasCABundle = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasCABundleMatchesExpectedCA(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"kind": "APIService",
		"spec": map[string]any{"caBundle": base64.StdEncoding.EncodeToString(testCA)},
	}}

	if !hasCABundle(obj, testCA) {
		t.Fatal("current CA bundle was rejected")
	}

	if hasCABundle(obj, []byte("different CA")) {
		t.Fatal("stale CA bundle was accepted")
	}

	obj.Object["spec"].(map[string]any)["caBundle"] = "not base64"
	if hasCABundle(obj, testCA) {
		t.Fatal("malformed CA bundle was accepted")
	}
}

// existingRegistrations builds the three registrations as they would already
// exist in a cluster, each carrying the given caBundle.
func existingRegistrations(t *testing.T, caBundle string) []client.Object {
	t.Helper()

	decoded, err := base64.StdEncoding.DecodeString(caBundle)
	if err != nil {
		t.Fatalf("decode caBundle fixture: %v", err)
	}

	clientConfig := admissionregistrationv1.WebhookClientConfig{
		Service:  &admissionregistrationv1.ServiceReference{Namespace: component.DefaultNamespace, Name: controllerName},
		CABundle: decoded,
	}

	sideEffects := admissionregistrationv1.SideEffectClassNone

	return []client.Object{
		&admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-validating-webhook"},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{{
				Name:                    "a.unbounded-cloud.io",
				ClientConfig:            clientConfig,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
			}},
		},
		&admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-mutating-webhook"},
			Webhooks: []admissionregistrationv1.MutatingWebhook{{
				Name:                    "b.unbounded-cloud.io",
				ClientConfig:            clientConfig,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
			}},
		},
		&apiregistrationv1.APIService{
			ObjectMeta: metav1.ObjectMeta{Name: "v1alpha1.status.net.unbounded-cloud.io"},
			Spec: apiregistrationv1.APIServiceSpec{
				Group:    "status.net.unbounded-cloud.io",
				Version:  "v1alpha1",
				CABundle: decoded,
				Service: &apiregistrationv1.ServiceReference{
					Namespace: component.DefaultNamespace,
					Name:      controllerName,
				},
			},
		},
	}
}

// caBundlesOf returns every CA bundle a registration carries.
func caBundlesOf(t *testing.T, obj *unstructured.Unstructured) []string {
	t.Helper()

	if obj.GetKind() == "APIService" {
		bundle, _, _ := unstructured.NestedString(obj.Object, "spec", "caBundle")

		return []string{bundle}
	}

	webhooks, _, _ := unstructured.NestedSlice(obj.Object, "webhooks")

	bundles := make([]string, 0, len(webhooks))

	for i := range webhooks {
		webhook, ok := webhooks[i].(map[string]any)
		if !ok {
			t.Fatalf("webhooks[%d] is not an object", i)
		}

		clientConfig, ok := webhook["clientConfig"].(map[string]any)
		if !ok {
			t.Fatalf("webhooks[%d] has no clientConfig", i)
		}

		bundle, _ := clientConfig["caBundle"].(string)
		bundles = append(bundles, bundle)
	}

	return bundles
}

// TestPlanGolden pins the complete set of operations the net component plans.
//
// Net is the cluster dataplane and applies the largest object set of any
// component, including the ValidatingAdmissionPolicy that restricts what its
// own ServiceAccount may create. The reaper gates its migration on the
// config-hash annotation the two workloads carry
// (internal/operator/migrate.go), so an object or annotation silently
// appearing, disappearing or being renamed here breaks the upgrade path.
//
// Both workloads depend on the config, so a failure to write the ConfigMap
// skips them rather than rolling pods that cannot mount it.
//
// The backend is seeded as serving so this stays a test about the full object
// set. TestPlanGoldenWithholdsRegistrationsWhileTheBackendIsDown pins the other
// half.
func TestPlanGolden(t *testing.T) {
	env := testEnv(t, servingObjects()...)

	plan, res, err := (Component{}).Plan(t.Context(), env, site())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	want := `CreateIfAbsent ConfigMap/unbounded-system/unbounded-net-config
Apply ServiceAccount/unbounded-system/unbounded-net-controller
Apply ServiceAccount/unbounded-system/unbounded-net-kube-proxy
Apply ClusterRole/unbounded-net-controller
Apply ClusterRoleBinding/unbounded-net-kube-proxy
Apply ClusterRoleBinding/unbounded-net-controller
Apply Role/unbounded-system/unbounded-net-controller
Apply RoleBinding/unbounded-system/unbounded-net-controller
Apply Role/kube-system/unbounded-net-controller
Apply RoleBinding/kube-system/unbounded-net-controller
Apply Deployment/unbounded-system/unbounded-net-controller [overridable] [after ConfigMap/unbounded-system/unbounded-net-config]
Apply Service/unbounded-system/unbounded-net-controller
Apply ValidatingWebhookConfiguration/unbounded-net-validating-webhook
Apply APIService/v1alpha1.status.net.unbounded-cloud.io
Apply MutatingWebhookConfiguration/unbounded-net-mutating-webhook
Apply ValidatingAdmissionPolicy/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicy/unbounded-net-node-field-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-node-field-restriction
Apply ValidatingAdmissionPolicy/unbounded-net-public-ip-annotation-ownership
Apply ValidatingAdmissionPolicyBinding/unbounded-net-public-ip-annotation-ownership
Apply ClusterRole/unbounded-net-status-viewer
Apply ServiceAccount/unbounded-system/unbounded-net-node
Apply ClusterRole/unbounded-net-node
Apply ClusterRoleBinding/unbounded-net-node
Apply DaemonSet/unbounded-system/unbounded-net-node [overridable] [after ConfigMap/unbounded-system/unbounded-net-config]
`

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestPlanGoldenWithholdsRegistrationsWhileTheBackendIsDown is the gate stated
// as what the pass writes.
//
// Withholding is a property of the plan rather than of a second apply, so it is
// visible here: exactly the three objects that route apiserver traffic at the
// controller Service are absent, and nothing else is. The three
// ValidatingAdmissionPolicy pairs stay, because the apiserver evaluates them
// itself and they have no backend to wait for.
func TestPlanGoldenWithholdsRegistrationsWhileTheBackendIsDown(t *testing.T) {
	env := testEnv(t)

	plan, res, err := (Component{}).Plan(t.Context(), env, site())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if res.Ready || res.Reason != component.ReasonBackendNotReady {
		t.Fatalf("result = %+v, want not ready with %q", res, component.ReasonBackendNotReady)
	}

	want := `CreateIfAbsent ConfigMap/unbounded-system/unbounded-net-config
Apply ServiceAccount/unbounded-system/unbounded-net-controller
Apply ServiceAccount/unbounded-system/unbounded-net-kube-proxy
Apply ClusterRole/unbounded-net-controller
Apply ClusterRoleBinding/unbounded-net-kube-proxy
Apply ClusterRoleBinding/unbounded-net-controller
Apply Role/unbounded-system/unbounded-net-controller
Apply RoleBinding/unbounded-system/unbounded-net-controller
Apply Role/kube-system/unbounded-net-controller
Apply RoleBinding/kube-system/unbounded-net-controller
Apply Deployment/unbounded-system/unbounded-net-controller [overridable] [after ConfigMap/unbounded-system/unbounded-net-config]
Apply Service/unbounded-system/unbounded-net-controller
Apply ValidatingAdmissionPolicy/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicy/unbounded-net-node-field-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-node-field-restriction
Apply ValidatingAdmissionPolicy/unbounded-net-public-ip-annotation-ownership
Apply ValidatingAdmissionPolicyBinding/unbounded-net-public-ip-annotation-ownership
Apply ClusterRole/unbounded-net-status-viewer
Apply ServiceAccount/unbounded-system/unbounded-net-node
Apply ClusterRole/unbounded-net-node
Apply ClusterRoleBinding/unbounded-net-node
Apply DaemonSet/unbounded-system/unbounded-net-node [overridable] [after ConfigMap/unbounded-system/unbounded-net-config]
`

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestExecutionOrderGolden pins the order the executor runs net's plan in, as
// distinct from the order the component emits it.
//
// Summary, which TestPlanGolden asserts on, renders emission order. The
// executor sorts a copy, so for a long time nothing pinned what the cluster
// actually sees, and execution order was changed twice without a single test
// noticing.
//
// Two properties here are load-bearing rather than incidental. The ConfigMap
// and Service precede both workloads, because pods mount one and resolve the
// other. Admission registration and the APIService come last, because each
// points at the controller Deployment: registering a failurePolicy: Ignore
// webhook before its backend exists is a window in which it silently enforces
// nothing. That ordering is why the registrations need no DependsOn of their
// own, and it is the half of the problem the backend gate does not solve:
// ordering says "after the apply returned", the gate says "after it is
// serving".
func TestExecutionOrderGolden(t *testing.T) {
	env := testEnv(t, servingObjects()...)

	plan, _, err := (Component{}).Plan(t.Context(), env, site())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	want := `Apply ServiceAccount/unbounded-system/unbounded-net-controller
Apply ServiceAccount/unbounded-system/unbounded-net-kube-proxy
Apply ClusterRole/unbounded-net-controller
Apply ClusterRoleBinding/unbounded-net-kube-proxy
Apply ClusterRoleBinding/unbounded-net-controller
Apply Role/unbounded-system/unbounded-net-controller
Apply RoleBinding/unbounded-system/unbounded-net-controller
Apply Role/kube-system/unbounded-net-controller
Apply RoleBinding/kube-system/unbounded-net-controller
Apply ClusterRole/unbounded-net-status-viewer
Apply ServiceAccount/unbounded-system/unbounded-net-node
Apply ClusterRole/unbounded-net-node
Apply ClusterRoleBinding/unbounded-net-node
CreateIfAbsent ConfigMap/unbounded-system/unbounded-net-config
Apply Service/unbounded-system/unbounded-net-controller
Apply Deployment/unbounded-system/unbounded-net-controller
Apply DaemonSet/unbounded-system/unbounded-net-node
Apply ValidatingWebhookConfiguration/unbounded-net-validating-webhook
Apply APIService/v1alpha1.status.net.unbounded-cloud.io
Apply MutatingWebhookConfiguration/unbounded-net-mutating-webhook
Apply ValidatingAdmissionPolicy/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicy/unbounded-net-node-field-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-node-field-restriction
Apply ValidatingAdmissionPolicy/unbounded-net-public-ip-annotation-ownership
Apply ValidatingAdmissionPolicyBinding/unbounded-net-public-ip-annotation-ownership
`

	got, err := plan.ExecutionOrder()
	if err != nil {
		t.Fatalf("ExecutionOrder: %v", err)
	}

	if got != want {
		t.Fatalf("execution order =\n%s\nwant\n%s", got, want)
	}
}
