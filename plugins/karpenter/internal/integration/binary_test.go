// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/provider"
)

var (
	nodePoolGVK  = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodePool"}
	nodeClaimGVK = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"}
)

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

// TestBinaryProvisionsAndRetiresThroughAllocations runs the built
// unbounded-karpenter binary under the plugin's service account identity
// against a real API server. The test plays the scheduler (an unschedulable
// Pod) and the allocation controller (snapshot, finalizer, release), and
// checks that the pinned upstream controllers and the plugin together turn
// the Pod into a NodeClaim and an allocation, record the launch at the
// snapshot, fence on termination, authorize release, and finish, without a
// single forbidden request.
func TestBinaryProvisionsAndRetiresThroughAllocations(t *testing.T) {
	binary := os.Getenv("UNBOUNDED_KARPENTER_BINARY")
	if binary == "" {
		t.Skip("set UNBOUNDED_KARPENTER_BINARY to the built unbounded-karpenter binary")
	}

	c := start(t)
	ctx := t.Context()
	class := c.readyClass()

	user, err := c.environment.AddUser(envtest.User{
		Name:   pluginUser,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
	}, nil)
	must(t, err)

	kubeconfig, err := user.KubeConfig()
	must(t, err)

	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	must(t, os.WriteFile(kubeconfigPath, kubeconfig, 0o600))

	output := new(syncBuffer)

	runContext, stop := context.WithCancel(ctx)
	command := exec.CommandContext(
		runContext, binary,
		"--leader-election-name=unbounded-karpenter",
		"--leader-election-namespace="+namespace,
		"--ignore-dra-requests=true",
		"--feature-gates=NodeRepair=false,ReservedCapacity=false,SpotToSpotConsolidation=false,NodeOverlay=false,StaticCapacity=false,CapacityBuffer=false",
		"--metrics-port=18080",
		"--health-probe-port=18081",
		"--batch-idle-duration=1s",
		"--batch-max-duration=2s",
	)
	command.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath, "SYSTEM_NAMESPACE="+namespace)
	command.Stdout = output
	command.Stderr = output
	must(t, command.Start())

	t.Cleanup(func() {
		stop()

		_ = command.Wait() //nolint:errcheck // The process is killed on purpose.

		if t.Failed() {
			t.Logf("unbounded-karpenter output:\n%s", output.String())
		}
	})

	pool := new(unstructured.Unstructured)
	pool.SetGroupVersionKind(nodePoolGVK)
	pool.SetName("workers")
	pool.Object["spec"] = map[string]any{
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"example.com/tier": "batch"}},
			"spec": map[string]any{
				"nodeClassRef": map[string]any{"group": "unbounded-cloud.io", "kind": "MachineClass", "name": "workers"},
				"requirements": []any{map[string]any{
					"key": "karpenter.sh/capacity-type", "operator": "In", "values": []any{"on-demand"},
				}},
				"expireAfter": "Never",
			},
		},
		"disruption": map[string]any{"consolidationPolicy": "WhenEmpty", "consolidateAfter": "Never"},
	}
	must(t, c.admin.Create(ctx, pool))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demand", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "demo",
			Image: "registry.k8s.io/pause:3.10",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			}},
		}}},
	}
	must(t, c.admin.Create(ctx, pod))

	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodScheduled,
		Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable,
	}}
	must(t, c.admin.Status().Update(ctx, pod))

	// Karpenter plans a NodeClaim and the plugin persists its allocation.
	var allocation *unstructured.Unstructured

	c.eventually("an allocation for a NodeClaim", 2*time.Minute, func() (bool, error) {
		list := new(unstructured.UnstructuredList)
		list.SetGroupVersionKind(capacity.AllocationGVK.GroupVersion().WithKind("MachineAllocationList"))

		if err := c.admin.List(ctx, list); err != nil {
			return false, err
		}

		if len(list.Items) == 0 {
			return false, nil
		}

		allocation = &list.Items[0]

		return true, nil
	})

	decoded, err := provider.DecodeAllocation(allocation)
	must(t, err)

	for _, requirement := range decoded.Spec.Requirements {
		if requirement.Key == "karpenter.sh/nodepool" || requirement.Key == "example.com/tier" ||
			requirement.Key == "karpenter.sh/capacity-type" {
			t.Fatalf("requirement on %s must not reach the allocation: %+v", requirement.Key, decoded.Spec.Requirements)
		}
	}

	if decoded.Spec.NodeTemplate.Labels["karpenter.sh/nodepool"] != "workers" ||
		decoded.Spec.NodeTemplate.Labels["example.com/tier"] != "batch" ||
		decoded.Spec.ClassRef.UID != class.GetUID() {
		t.Fatalf("unexpected allocation from the real scheduler: %+v", decoded.Spec)
	}

	claimName := decoded.Spec.ConsumerRef.Name

	// The snapshot alone makes Karpenter record the launch.
	c.acceptFrom(allocation.GetName(), string(class.GetUID()))

	nodeClaim := new(unstructured.Unstructured)
	nodeClaim.SetGroupVersionKind(nodeClaimGVK)

	c.eventually("the NodeClaim to be launched", time.Minute, func() (bool, error) {
		if err := c.admin.Get(ctx, client.ObjectKey{Name: claimName}, nodeClaim); err != nil {
			return false, err
		}

		providerID, _, _ := unstructured.NestedString(nodeClaim.Object, "status", "providerID")

		return providerID == capacity.ProviderIDPrefix+string(allocation.GetUID()), nil
	})

	if labels := nodeClaim.GetLabels(); labels["topology.kubernetes.io/zone"] != "zone-1" ||
		labels["kubernetes.io/os"] != "linux" || labels["node.kubernetes.io/instance-type"] != "test.medium" {
		t.Fatalf("the launched NodeClaim lost its resolved labels: %v", labels)
	}

	// Termination: the plugin fences by deleting the allocation, Karpenter
	// calls Delete, and the plugin authorizes release.
	must(t, c.admin.Delete(ctx, nodeClaim))

	stored := provider.AllocationObject()

	c.eventually("the allocation to be fenced and its release authorized", time.Minute, func() (bool, error) {
		if err := c.admin.Get(ctx, client.ObjectKey{Name: allocation.GetName()}, stored); err != nil {
			return false, err
		}

		lifecycle, _, _ := unstructured.NestedString(stored.Object, "spec", "lifecycle")

		return stored.GetDeletionTimestamp() != nil && lifecycle == capacity.Released, nil
	})

	if err := c.admin.Get(ctx, client.ObjectKey{Name: claimName}, nodeClaim); err != nil {
		t.Fatalf("the NodeClaim must wait for the release to be confirmed: %v", err)
	}

	// The allocation controller confirms the release.
	stored.SetFinalizers(nil)
	must(t, c.admin.Update(ctx, stored))

	c.eventually("the NodeClaim to finish", time.Minute, func() (bool, error) {
		err := c.admin.Get(ctx, client.ObjectKey{Name: claimName}, nodeClaim)

		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})

	for line := range strings.SplitSeq(output.String(), "\n") {
		if strings.Contains(line, "forbidden") || strings.Contains(line, "Forbidden") {
			t.Errorf("the plugin's roles are missing a permission: %s", line)
		}
	}
}

func (c *cluster) eventually(what string, timeout time.Duration, condition func() (bool, error)) {
	c.t.Helper()

	err := wait.PollUntilContextTimeout(c.t.Context(), 500*time.Millisecond, timeout, true,
		func(context.Context) (bool, error) { return condition() })
	if err != nil {
		c.t.Fatalf("waiting for %s: %v", what, err)
	}
}
