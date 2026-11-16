// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	provisioningscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
)

func TestPinnedSchedulerScalesCapturedWorkerPodFromZero(t *testing.T) {
	class, pool := capturedSchedulingInputs(t)
	pod := workerDemoPod(t)
	result := simulateCapturedScheduling(t, class, pool, pod)

	if err := result.PodErrors[pod]; err != nil {
		t.Fatalf("captured worker pod must pass the pinned upstream provisioner filter: %v", err)
	}

	if len(result.NewNodeClaims) != 1 {
		t.Fatalf("expected one planned NodeClaim from zero, got %d", len(result.NewNodeClaims))
	}

	claim := result.NewNodeClaims[0].ToNodeClaim()
	if claim.Labels[v1.NodePoolLabelKey] != pod.Spec.NodeSelector[v1.NodePoolLabelKey] ||
		claim.Labels["unbounded-cloud.io/poc"] != pool.Spec.Template.Labels["unbounded-cloud.io/poc"] {
		t.Fatal("upstream scheduling must preserve workload isolation and template labels")
	}
}

func TestPinnedSchedulerRetainsCustomProviderConstraints(t *testing.T) {
	const key = "inventory.example.com/rack"

	for _, test := range []struct {
		name     string
		required string
		wantNode bool
	}{
		{
			name:     "undeclared provider label does not block an eligible offering",
			wantNode: true,
		},
		{
			name:     "matching declared provider label remains eligible",
			required: "rack-one",
			wantNode: true,
		},
		{
			name:     "conflicting declared provider label remains ineligible",
			required: "rack-two",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			class, pool := capturedSchedulingInputs(t)

			class.Status.Offerings[0].Labels[key] = "rack-one"
			if test.required != "" {
				pool.Spec.Template.Spec.Requirements = append(
					pool.Spec.Template.Spec.Requirements,
					v1.NodeSelectorRequirementWithMinValues{
						Key:      key,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{test.required},
					},
				)
			}

			pod := workerDemoPod(t)
			result := simulateCapturedScheduling(t, class, pool, pod)
			assertSchedulingOutcome(t, result, pod, test.wantNode)

			if test.wantNode &&
				!result.NewNodeClaims[0].InstanceTypeOptions[0].Requirements.Get(key).Has("rack-one") {
				t.Fatal("provider labels must remain available for instance-type matching")
			}
		})
	}
}

func TestPinnedSchedulerDoesNotRelaxCapturedCallerRestrictions(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*nodeclass.MachineClass, *v1.NodePool, *corev1.Pod)
	}{
		{
			name: "workload targets a different NodePool",
			change: func(_ *nodeclass.MachineClass, _ *v1.NodePool, pod *corev1.Pod) {
				pod.Spec.NodeSelector[v1.NodePoolLabelKey] = "other-pool"
			},
		},
		{
			name: "provider label conflicts with a template label",
			change: func(class *nodeclass.MachineClass, _ *v1.NodePool, _ *corev1.Pod) {
				class.Status.Offerings[0].Labels["unbounded-cloud.io/poc"] = "other-purpose"
			},
		},
		{
			name: "requested zone is unavailable",
			change: func(_ *nodeclass.MachineClass, _ *v1.NodePool, pod *corev1.Pod) {
				pod.Spec.NodeSelector[corev1.LabelTopologyZone] = "other-zone"
			},
		},
		{
			name: "requested capacity type is unavailable",
			change: func(_ *nodeclass.MachineClass, _ *v1.NodePool, pod *corev1.Pod) {
				pod.Spec.NodeSelector[v1.CapacityTypeLabelKey] = v1.CapacityTypeSpot
			},
		},
		{
			name: "offering is unknown",
			change: func(class *nodeclass.MachineClass, _ *v1.NodePool, _ *corev1.Pod) {
				class.Status.Offerings[0].Availability = "Unknown"
			},
		},
		{
			name: "offering has expired",
			change: func(class *nodeclass.MachineClass, _ *v1.NodePool, _ *corev1.Pod) {
				class.Status.Offerings[0].ValidUntil = class.Status.Offerings[0].ObservedAt.DeepCopy()
			},
		},
		{
			name: "pod resources exceed capacity",
			change: func(_ *nodeclass.MachineClass, _ *v1.NodePool, pod *corev1.Pod) {
				pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1000")
			},
		},
		{
			name: "zone and capacity type cannot be combined across offerings",
			change: func(class *nodeclass.MachineClass, pool *v1.NodePool, _ *corev1.Pod) {
				offering := class.Status.Offerings[0]
				offering.Labels = maps.Clone(offering.Labels)
				offering.Name += "-other-spot"
				offering.Zone = "other-zone"
				offering.CapacityType = "Spot"
				offering.Labels[corev1.LabelTopologyZone] = offering.Zone
				offering.Labels[capacity.CapacityTypeLabel] = offering.CapacityType
				class.Status.Offerings = append(class.Status.Offerings, offering)

				for index := range pool.Spec.Template.Spec.Requirements {
					requirement := &pool.Spec.Template.Spec.Requirements[index]
					if requirement.Key == v1.CapacityTypeLabelKey {
						requirement.Values = []string{v1.CapacityTypeSpot}
					}
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			class, pool := capturedSchedulingInputs(t)
			pod := workerDemoPod(t)
			test.change(class, pool, pod)
			result := simulateCapturedScheduling(t, class, pool, pod)
			assertSchedulingOutcome(t, result, pod, false)
		})
	}
}

func assertSchedulingOutcome(t *testing.T, result provisioningscheduling.Results, pod *corev1.Pod, wantNode bool) {
	t.Helper()

	if wantNode {
		if result.PodErrors[pod] != nil || len(result.NewNodeClaims) != 1 {
			t.Fatalf("expected one planned NodeClaim: %v", result.PodErrors[pod])
		}

		return
	}

	if result.PodErrors[pod] == nil || len(result.NewNodeClaims) != 0 {
		t.Fatal("upstream must reject the ineligible offering without relaxing caller restrictions")
	}
}

func simulateCapturedScheduling(
	t *testing.T,
	class *nodeclass.MachineClass,
	pool *v1.NodePool,
	pod *corev1.Pod,
) provisioningscheduling.Results {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)

	ctx = options.ToContext(ctx, &options.Options{IgnoreDRARequests: true})
	now := class.Status.Offerings[0].ObservedAt.Add(time.Second)
	clock := clocktesting.NewFakeClock(now)
	provider, store := testProvider(t, class)
	provider.now = clock.Now
	instanceTypes, err := provider.GetInstanceTypes(ctx, pool)
	must(t, err)

	byPool := map[string][]*cloudprovider.InstanceType{
		pool.Name: instanceTypes,
	}
	cluster := state.NewCluster(clock, store, provider)
	topology, err := provisioningscheduling.NewTopology(
		ctx, store, cluster, nil, []*v1.NodePool{pool}, byPool, []*corev1.Pod{pod},
	)
	must(t, err)

	scheduler := provisioningscheduling.NewScheduler(
		ctx, store, []*v1.NodePool{pool}, cluster, nil, topology, byPool, nil,
		events.NewRecorder(record.NewFakeRecorder(100)), clock, nil, nil,
	)
	result, err := scheduler.Solve(ctx, []*corev1.Pod{pod})
	must(t, err)

	return result
}

func capturedSchedulingInputs(t *testing.T) (*nodeclass.MachineClass, *v1.NodePool) {
	t.Helper()

	directory := os.Getenv("UNBOUNDED_KARPENTER_REPLAY_DIRECTORY")
	if directory == "" {
		directory = "testdata"
	}

	class := new(nodeclass.MachineClass)

	pool := new(v1.NodePool)
	for _, input := range []struct {
		name   string
		target any
	}{
		{
			name:   "live-machineclass.json",
			target: class,
		},
		{
			name:   "live-nodepool.json",
			target: pool,
		},
	} {
		content, err := os.ReadFile(filepath.Join(directory, input.name))
		must(t, err)
		must(t, json.Unmarshal(content, input.target))
	}

	return class, pool
}

func workerDemoPod(t *testing.T) *corev1.Pod {
	t.Helper()

	path := filepath.Join("testdata", "workload.yaml")
	content, err := os.ReadFile(path)
	must(t, err)

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)

	for {
		deployment := new(appsv1.Deployment)

		err := decoder.Decode(deployment)
		if err == io.EOF {
			t.Fatal("worker-demo Deployment is missing")
		}

		must(t, err)

		if deployment.Kind != "Deployment" {
			continue
		}

		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployment.Name,
				Namespace: deployment.Namespace,
				UID:       types.UID(deployment.Name + "-replay"),
				Labels:    deployment.Spec.Template.Labels,
			},
			Spec: deployment.Spec.Template.Spec,
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
			},
		}
	}
}
