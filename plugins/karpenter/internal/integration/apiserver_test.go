// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package integration runs the plugin against a real API server with the
// generated capacity CRDs, the plugin's rendered roles and its admission
// policy. It proves what a fake client cannot: that the API server accepts
// every write the plugin makes under its own identity, and refuses the ones
// it must not make.
package integration

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/manifests"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
	"github.com/Azure/unbounded/plugins/karpenter/internal/provider"
)

const (
	namespace           = "unbounded-system"
	pluginUser          = "system:serviceaccount:" + namespace + ":unbounded-karpenter"
	allocationFinalizer = "unbounded-cloud.io/machineallocation"
)

var classGVK = schema.GroupVersionKind{Group: "unbounded-cloud.io", Version: "v1alpha3", Kind: "MachineClass"}

type cluster struct {
	t           *testing.T
	environment *envtest.Environment
	// admin stands in for the operator and the allocation controller.
	admin client.Client
	// plugin acts as the plugin's service account, bound by the rendered
	// roles and admission policy.
	plugin client.Client
}

func start(t *testing.T) *cluster {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API server tests")
	}

	rendered, err := manifests.Render(manifests.Options{
		Namespace: namespace,
		Image:     "registry.example.com/unbounded/unbounded-karpenter:test",
	})
	must(t, err)

	var (
		crds    []*apiextensionsv1.CustomResourceDefinition
		objects []*unstructured.Unstructured
	)

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)

	for {
		object := new(unstructured.Unstructured)
		if err := decoder.Decode(object); err != nil {
			if err == io.EOF {
				break
			}

			must(t, err)
		}

		switch object.GetKind() {
		case "":
		case "CustomResourceDefinition":
			crd := new(apiextensionsv1.CustomResourceDefinition)
			must(t, runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, crd))
			crds = append(crds, crd)
		case "Deployment":
		default:
			objects = append(objects, object)
		}
	}

	environment := &envtest.Environment{
		CRDs:                  crds,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "..", "deploy", "machina", "crd")},
		ErrorIfCRDPathMissing: true,
	}

	config, err := environment.Start()
	must(t, err)
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Logf("stop API server: %v", err)
		}
	})

	scheme := runtime.NewScheme()
	must(t, corev1.AddToScheme(scheme))
	nodeclass.AddToScheme(scheme)

	admin, err := client.New(config, client.Options{Scheme: scheme})
	must(t, err)

	ctx := t.Context()

	for _, object := range objects {
		if err := admin.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %s %s: %v", object.GetKind(), object.GetName(), err)
		}
	}

	impersonated := rest.CopyConfig(config)
	impersonated.Impersonate = rest.ImpersonationConfig{
		UserName: pluginUser,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
	}

	plugin, err := client.New(impersonated, client.Options{Scheme: scheme})
	must(t, err)

	c := &cluster{t: t, environment: environment, admin: admin, plugin: plugin}
	c.awaitPolicy()

	return c
}

// awaitPolicy waits until the admission policy is enforced: the API server
// compiles a new policy and its binding asynchronously.
func (c *cluster) awaitPolicy() {
	c.t.Helper()

	ctx := c.t.Context()

	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		probe := allocation("policy-probe", "MachinePool", "policy-probe", "00000000-0000-4000-8000-000000000000")

		err := c.plugin.Create(ctx, probe)
		if err == nil {
			return false, c.admin.Delete(ctx, probe)
		}

		return apierrors.IsForbidden(err) || apierrors.IsInvalid(err), nil
	})
	if err != nil {
		c.t.Fatalf("the admission policy never took effect: %v", err)
	}
}

func allocation(name, consumerKind, consumerName, consumerUID string) *unstructured.Unstructured {
	object := provider.AllocationObject()
	object.SetName(name)
	object.Object["spec"] = map[string]any{
		"consumerRef": map[string]any{
			"apiGroup": capacity.ConsumerGroup,
			"kind":     consumerKind,
			"name":     consumerName,
			"uid":      consumerUID,
		},
		"classRef": map[string]any{"name": "workers", "uid": "00000000-0000-4000-8000-00000000c1a5"},
	}

	return object
}

// readyClass creates a MachineClass and gives it the status the class
// controller publishes once it resolved the class.
func (c *cluster) readyClass() *unstructured.Unstructured {
	c.t.Helper()

	ctx := c.t.Context()

	class := new(unstructured.Unstructured)
	class.SetGroupVersionKind(classGVK)
	class.SetName("workers")
	class.Object["spec"] = map[string]any{
		"siteRef":           map[string]any{"name": "test-site"},
		"provider":          "Test",
		"providerConfigRef": map[string]any{"kind": "ConfigMap", "namespace": namespace, "name": "workers-v1"},
		"configurationRef":  map[string]any{"name": "linux", "version": int64(1)},
	}
	must(c.t, c.admin.Create(ctx, class))

	now := metav1.Now().UTC().Format(time.RFC3339)
	class.Object["status"] = map[string]any{
		"observedGeneration": class.GetGeneration(),
		"resolvedRevision":   "revision-1",
		"offerings":          []any{offering(now)},
		"conditions": []any{map[string]any{
			"type":               "Ready",
			"status":             "True",
			"reason":             "Resolved",
			"message":            "resolved",
			"observedGeneration": class.GetGeneration(),
			"lastTransitionTime": now,
		}},
	}
	must(c.t, c.admin.Status().Update(ctx, class))

	return class
}

func offering(now string) map[string]any {
	return map[string]any{
		"name":         "medium-zone-one",
		"instanceType": "test.medium",
		"zone":         "zone-1",
		"capacityType": "OnDemand",
		"availability": "Available",
		"labels":       map[string]any{corev1.LabelArchStable: "amd64"},
		"capacity":     map[string]any{"cpu": "4", "memory": "16Gi", "pods": "110"},
		"observedAt":   now,
		"validUntil":   metav1.NewTime(time.Now().Add(time.Hour)).UTC().Format(time.RFC3339),
	}
}

func claim() *v1.NodeClaim {
	return &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "workers-abcde",
			UID:    "55361535-e293-488e-aa38-cef580e84311",
			Labels: map[string]string{v1.NodePoolLabelKey: "workers"},
		},
		Spec: v1.NodeClaimSpec{
			NodeClassRef: &v1.NodeClassReference{Group: "unbounded-cloud.io", Kind: "MachineClass", Name: "workers"},
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{Key: v1.NodePoolLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"workers"}},
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"test.medium"}},
				{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"on-demand"}},
			},
			Resources: v1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			}},
			Taints: []corev1.Taint{{Key: "dedicated", Value: "batch", Effect: corev1.TaintEffectNoSchedule}},
		},
	}
}

// accept plays the allocation controller: one status write freezes the
// snapshot, then the finalizer is added.
func (c *cluster) accept(name string) *unstructured.Unstructured {
	c.t.Helper()

	return c.acceptFrom(name, "00000000-0000-4000-8000-00000000c1a5")
}

// acceptFrom is accept with the snapshot taken from the class with classUID.
func (c *cluster) acceptFrom(name, classUID string) *unstructured.Unstructured {
	c.t.Helper()

	ctx := c.t.Context()

	object := provider.AllocationObject()
	must(c.t, c.admin.Get(ctx, client.ObjectKey{Name: name}, object))

	uid := string(object.GetUID())
	now := metav1.Now().UTC().Format(time.RFC3339)
	selected := offering(now)

	object.Object["status"] = map[string]any{
		"observedGeneration": object.GetGeneration(),
		"providerID":         capacity.ProviderIDPrefix + uid,
		"instanceType":       "test.medium",
		"zone":               "zone-1",
		"capacityType":       "OnDemand",
		"image":              "host-image",
		"capacity":           map[string]any{"cpu": "4", "memory": "16Gi", "pods": "110"},
		"allocatable":        map[string]any{"cpu": "4", "memory": "16Gi", "pods": "110"},
		"labels": map[string]any{
			corev1.LabelInstanceTypeStable: "test.medium",
			corev1.LabelTopologyZone:       "zone-1",
			corev1.LabelArchStable:         "amd64",
			capacity.CapacityTypeLabel:     "OnDemand",
			v1.NodePoolLabelKey:            "workers",
		},
		"snapshot": map[string]any{
			"classUID":        classUID,
			"classGeneration": int64(1),
			"classRevision":   "revision-1",
			"provider":        "Test",
			"siteName":        "test-site",
			"siteUID":         "00000000-0000-4000-8000-0000000051e0",
			"credential":      map[string]any{"name": "credential", "uid": "00000000-0000-4000-8000-0000000c4ed0"},
			"offering":        selected,
			"providerConfig": map[string]any{
				"namespace": namespace, "name": "workers-v1", "uid": "00000000-0000-4000-8000-00000000c0f1", "hash": "hash",
			},
			"configuration": map[string]any{
				"name": "linux", "version": int64(1), "versionName": "linux-v1",
				"uid": "00000000-0000-4000-8000-00000000c0f2", "hash": "hash",
			},
			"configurationTemplate": map[string]any{},
			"hostImage":             "host-image",
			"hostProviderID":        "test://hosts/" + uid,
		},
	}
	must(c.t, c.admin.Status().Update(ctx, object))

	object.SetFinalizers([]string{allocationFinalizer})
	object.SetLabels(map[string]string{capacity.AllocationLabel: uid})
	must(c.t, c.admin.Update(ctx, object))

	return object
}

// coldCache reads objects by name from the API server and returns nothing
// for a field-indexed list, which only a real cache can answer.
type coldCache struct {
	client.Reader
}

func (r coldCache) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	applied := new(client.ListOptions)
	applied.ApplyOptions(options)

	if applied.FieldSelector != nil && !applied.FieldSelector.Empty() {
		return nil
	}

	return r.Reader.List(ctx, list, options...)
}

func (c *cluster) provider() *provider.Provider {
	c.t.Helper()

	// The plugin's client reads the API server directly. As the cache it
	// answers indexed lookups with nothing, like a cache that has not seen
	// the object yet, so the provider's confirming reads are exercised.
	instance, err := provider.New(coldCache{Reader: c.plugin}, c.plugin, c.plugin, provider.Options{SnapshotWait: -1})
	must(c.t, err)

	return instance
}

func TestPluginLifecycleAgainstTheAPIServer(t *testing.T) {
	c := start(t)
	ctx := t.Context()
	class := c.readyClass()
	instance := c.provider()
	nodeClaim := claim()
	name := capacity.AllocationName(nodeClaim.UID)

	// The plugin lists instance types from the class it may only read.
	pool := &v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{
		Spec: v1.NodeClaimTemplateSpec{NodeClassRef: nodeClaim.Spec.NodeClassRef},
	}}}

	types, err := instance.GetInstanceTypes(ctx, pool)
	must(t, err)

	if len(types) != 1 || types[0].Name != "test.medium" || len(types[0].Offerings) != 1 || !types[0].Offerings[0].Available {
		t.Fatalf("unexpected instance types %+v", types)
	}

	// Create persists the allocation under the plugin's identity, through
	// the schema and the admission policy, and reports it pending.
	if _, err := instance.Create(ctx, nodeClaim); err == nil {
		t.Fatal("an allocation without a snapshot must be pending")
	}

	stored := provider.AllocationObject()
	must(t, c.admin.Get(ctx, client.ObjectKey{Name: name}, stored))

	decoded, err := provider.DecodeAllocation(stored)
	must(t, err)

	if decoded.Spec.ClassRef.UID != class.GetUID() || decoded.Spec.Lifecycle != capacity.Active ||
		decoded.Spec.NodeTemplate.Labels[v1.NodePoolLabelKey] != "workers" {
		t.Fatalf("unexpected stored allocation %+v", decoded.Spec)
	}

	// The requirements describe the chosen offering by its own labels. The
	// NodePool requirement names a label only the Node has and is not sent.
	sent := map[string]string{}
	for _, requirement := range decoded.Spec.Requirements {
		sent[requirement.Key] = strings.Join(requirement.Values, ",")
	}

	if _, found := sent[v1.NodePoolLabelKey]; found || sent[corev1.LabelInstanceTypeStable] != "test.medium" ||
		sent[corev1.LabelTopologyZone] != "zone-1" || sent[capacity.CapacityTypeLabel] != "OnDemand" {
		t.Fatalf("unexpected stored requirements %+v", decoded.Spec.Requirements)
	}

	// The snapshot is enough for Karpenter to record the launch.
	accepted := c.accept(name)

	hydrated, err := instance.Create(ctx, nodeClaim)
	must(t, err)

	providerID := capacity.ProviderIDPrefix + string(accepted.GetUID())
	if hydrated.Status.ProviderID != providerID || hydrated.Labels[v1.CapacityTypeLabelKey] != v1.CapacityTypeOnDemand {
		t.Fatalf("unexpected hydrated NodeClaim %+v", hydrated)
	}

	found, err := instance.Get(ctx, providerID)
	must(t, err)

	if found.Name != nodeClaim.Name {
		t.Fatal("Get returned another NodeClaim")
	}

	reason, err := instance.IsDrifted(ctx, nodeClaim)
	must(t, err)

	if reason != provider.DriftClassReplaced {
		// The snapshot above names a synthetic class UID, so the real class
		// is a different incarnation.
		t.Fatalf("expected the class to be reported replaced, got %q", reason)
	}

	// Fence: the plugin deletes the allocation; the finalizer keeps it.
	must(t, c.plugin.Delete(ctx, accepted))
	must(t, c.admin.Get(ctx, client.ObjectKey{Name: name}, stored))

	if stored.GetDeletionTimestamp() == nil {
		t.Fatal("the finalizer must keep the fenced allocation")
	}

	// Release can be authorized on the deleting allocation, and that is not
	// yet confirmed absence.
	nodeClaim.Status.ProviderID = providerID
	must(t, instance.Delete(ctx, nodeClaim))
	must(t, c.admin.Get(ctx, client.ObjectKey{Name: name}, stored))

	if lifecycle, _, _ := unstructured.NestedString(stored.Object, "spec", "lifecycle"); lifecycle != capacity.Released {
		t.Fatal("release must be authorized on a deleting allocation")
	}

	if _, err := instance.Get(ctx, providerID); err != nil {
		t.Fatalf("an unconfirmed release must stay visible: %v", err)
	}

	// The allocation controller confirms release and removes its finalizer.
	stored.SetFinalizers(nil)
	must(t, c.admin.Update(ctx, stored))

	if err := instance.Delete(ctx, nodeClaim); !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("a released allocation must be reported not found, got %v", err)
	}
}

func TestPluginAuthorityIsBounded(t *testing.T) {
	c := start(t)
	ctx := t.Context()

	c.readyClass()

	// An allocation an operator owns.
	foreign := allocation("operator-owned", "MachinePool", "batch", "00000000-0000-4000-8000-00000000f00d")
	must(t, c.admin.Create(ctx, foreign))

	for name, write := range map[string]func() error{
		"create for another consumer kind": func() error {
			return c.plugin.Create(ctx, allocation("pool-allocation", "MachinePool", "batch", "00000000-0000-4000-8000-00000000f00d"))
		},
		"create under a name not derived from the consumer UID": func() error {
			return c.plugin.Create(ctx, allocation("nodeclaim-other", capacity.ConsumerKind, "claim", "55361535-e293-488e-aa38-cef580e84311"))
		},
		"release another consumer's allocation": func() error {
			return provider.AuthorizeRelease(ctx, c.plugin, foreign.DeepCopy())
		},
		"delete another consumer's allocation": func() error {
			return c.plugin.Delete(ctx, foreign.DeepCopy())
		},
		"write allocation status": func() error {
			object := foreign.DeepCopy()
			object.Object["status"] = map[string]any{"failureReason": "ProviderFailed"}

			return c.plugin.Status().Update(ctx, object)
		},
		"write a MachineClass": func() error {
			class := new(unstructured.Unstructured)
			class.SetGroupVersionKind(classGVK)
			class.SetName("workers")

			return c.plugin.Delete(ctx, class)
		},
		"read a Secret": func() error {
			return c.plugin.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "any"}, new(corev1.Secret))
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(); !apierrors.IsForbidden(err) {
				t.Fatalf("expected the API server to forbid it, got %v", err)
			}
		})
	}

	stored := provider.AllocationObject()
	must(t, c.admin.Get(ctx, client.ObjectKey{Name: "operator-owned"}, stored))

	if lifecycle, _, _ := unstructured.NestedString(stored.Object, "spec", "lifecycle"); lifecycle != capacity.Active ||
		stored.GetDeletionTimestamp() != nil {
		t.Fatal("the operator's allocation must be untouched")
	}

	// The plugin's own allocation is allowed, and its spec stays immutable.
	own := allocation(capacity.AllocationName("55361535-e293-488e-aa38-cef580e84311"), capacity.ConsumerKind, "claim",
		"55361535-e293-488e-aa38-cef580e84311")
	must(t, c.plugin.Create(ctx, own))
	must(t, provider.AuthorizeRelease(ctx, c.plugin, own))

	reactivate := own.DeepCopy()
	must(t, unstructured.SetNestedField(reactivate.Object, capacity.Active, "spec", "lifecycle"))

	if err := c.plugin.Patch(ctx, reactivate, client.MergeFrom(own)); !apierrors.IsInvalid(err) {
		t.Fatalf("lifecycle must not return to Active, got %v", err)
	}

	// The allocation controller's finalizer keeps an allocation until its
	// release is confirmed. The plugin must not be able to remove it.
	held := provider.AllocationObject()
	must(t, c.admin.Get(ctx, client.ObjectKey{Name: own.GetName()}, held))
	held.SetFinalizers([]string{"unbounded-cloud.io/machineallocation"})
	must(t, c.admin.Update(ctx, held))

	stripped := held.DeepCopy()
	stripped.SetFinalizers(nil)

	if err := c.plugin.Patch(ctx, stripped, client.MergeFrom(held)); !apierrors.IsForbidden(err) {
		t.Fatalf("the plugin must not remove allocation finalizers, got %v", err)
	}

	// Fencing by deletion is still allowed, and the finalizer holds.
	must(t, c.plugin.Delete(ctx, held.DeepCopy()))
	must(t, c.admin.Get(ctx, client.ObjectKey{Name: own.GetName()}, held))

	if held.GetDeletionTimestamp() == nil || len(held.GetFinalizers()) != 1 {
		t.Fatal("a deleted allocation must be kept by its finalizer")
	}
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
