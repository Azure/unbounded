// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/event"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/workload"
)

func racerCache() *racerv1.ClusterCache {
	return &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: RacerCacheName}}
}

// racerDataplane returns a dataplane DaemonSet whose placement differs from
// the Gantry manifest in every copied field.
func racerDataplane() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: workload.DataplaneDaemonSetName},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Affinity: &corev1.Affinity{
				NodeAffinity: workload.DataplaneAffinity().NodeAffinity,
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname"}},
				},
			},
			NodeSelector: map[string]string{"pool": "cache"},
			Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "cache", Effect: corev1.TaintEffectNoSchedule}},
		}}},
	}
}

func agentOperation(t *testing.T, plan *component.Plan) (component.Operation, bool) {
	t.Helper()

	for _, op := range plan.Operations {
		if op.Object.GetKind() == "DaemonSet" && op.Object.GetName() == daemonSetName {
			return op, true
		}
	}

	return component.Operation{}, false
}

func agentDaemonSet(t *testing.T, plan *component.Plan) (*appsv1.DaemonSet, component.Operation) {
	t.Helper()

	op, ok := agentOperation(t, plan)
	if !ok {
		t.Fatalf("plan has no DaemonSet/%s:\n%s", daemonSetName, plan.Summary())
	}

	ds := &appsv1.DaemonSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, ds); err != nil {
		t.Fatalf("convert DaemonSet: %v", err)
	}

	return ds, op
}

func agentContainer(t *testing.T, ds *appsv1.DaemonSet) corev1.Container {
	t.Helper()

	for _, container := range ds.Spec.Template.Spec.Containers {
		if container.Name == agentContainerName {
			return container
		}
	}

	t.Fatalf("DaemonSet has no %s container", agentContainerName)

	return corev1.Container{}
}

func racerEnv(container corev1.Container) (string, bool) {
	for _, env := range container.Env {
		if env.Name == racerEnabledEnv {
			return env.Value, true
		}
	}

	return "", false
}

func planGantry(t *testing.T, env *component.Env, sites ...unboundedv1alpha3.Site) (*component.Plan, component.Result) {
	t.Helper()

	plan, res, err := (Component{}).Plan(t.Context(), env, sites)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	return plan, res
}

func TestRacerCacheNameMatchesAgent(t *testing.T) {
	if RacerCacheName != gantryracer.CacheName {
		t.Fatalf("RacerCacheName = %q, Gantry agent serves %q", RacerCacheName, gantryracer.CacheName)
	}
}

func TestPlanWithoutRacerCacheKeepsDefaultMode(t *testing.T) {
	env := testEnv(t, racerDataplane())

	plan, res := planGantry(t, env, *siteWithGantry("edge", nil))
	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	ds, _ := agentDaemonSet(t, plan)
	if _, ok := racerEnv(agentContainer(t, ds)); ok {
		t.Fatalf("%s set without ClusterCache/%s", racerEnabledEnv, RacerCacheName)
	}

	if ds.Spec.Template.Spec.NodeSelector["pool"] == "cache" {
		t.Fatal("placement copied from racer-dataplane without ClusterCache/gantry")
	}
}

func TestPlanWithRacerCacheConfiguresRacerBackend(t *testing.T) {
	env := testEnv(t, racerCache(), racerDataplane())

	plan, res := planGantry(t, env, *siteWithGantry("edge", nil))
	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	ds, op := agentDaemonSet(t, plan)
	if !op.Overridable {
		t.Fatal("Racer-mode DaemonSet must stay overridable")
	}

	container := agentContainer(t, ds)
	if value, ok := racerEnv(container); !ok || value != "true" {
		t.Fatalf("%s = %q (set=%t), want true", racerEnabledEnv, value, ok)
	}

	// The container image and config hash are still stamped in Racer mode.
	if container.Image != env.Config.Image(imageRepository) {
		t.Fatalf("agent image = %q", container.Image)
	}

	if ds.Spec.Template.Annotations[configHashAnnotation] == "" {
		t.Fatal("config hash annotation missing in Racer mode")
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount
	}

	if got := mounts[racerClientVolume]; got.MountPath != "/run/racer/gantry/client" || !got.ReadOnly {
		t.Fatalf("client mount = %+v", got)
	}

	if got := mounts[racerOriginVolume]; got.MountPath != "/run/racer/gantry/origin" || got.ReadOnly {
		t.Fatalf("origin mount = %+v", got)
	}

	// The default mounts remain.
	if _, ok := mounts["config"]; !ok {
		t.Fatalf("config mount dropped: %+v", container.VolumeMounts)
	}

	volumes := map[string]corev1.Volume{}
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		volumes[volume.Name] = volume
	}

	for name, path := range map[string]string{racerClientVolume: "/run/racer/gantry/client", racerOriginVolume: "/run/racer/gantry/origin"} {
		hostPath := volumes[name].HostPath
		if hostPath == nil || hostPath.Path != path || ptr.Deref(hostPath.Type, "") != corev1.HostPathDirectory {
			t.Fatalf("volume %s = %+v, want hostPath %s type Directory", name, volumes[name], path)
		}
	}

	pod := ds.Spec.Template.Spec
	if len(pod.NodeSelector) != 1 || pod.NodeSelector["pool"] != "cache" {
		t.Fatalf("nodeSelector = %#v, want racer-dataplane selector", pod.NodeSelector)
	}

	if len(pod.Tolerations) != 1 || pod.Tolerations[0].Key != "dedicated" {
		t.Fatalf("tolerations = %#v, want racer-dataplane tolerations", pod.Tolerations)
	}

	if pod.Affinity == nil || pod.Affinity.NodeAffinity == nil || pod.Affinity.PodAntiAffinity != nil {
		t.Fatalf("affinity = %#v, want racer-dataplane node affinity only", pod.Affinity)
	}
}

func TestPlanWithRacerCacheDoesNotInstallGantry(t *testing.T) {
	no := false
	env := testEnv(t, racerCache(), racerDataplane())

	plan, res := planGantry(t, env, *siteWithGantry("edge", &no))
	if res.Reason != component.ReasonDisabled {
		t.Fatalf("result = %+v, want disabled", res)
	}

	if _, ok := agentOperation(t, plan); ok {
		t.Fatal("ClusterCache/gantry installed Gantry without an enabling Site")
	}

	plan, res = planGantry(t, env)
	if res.Reason != component.ReasonDisabled {
		t.Fatalf("result without Sites = %+v, want disabled", res)
	}

	if _, ok := agentOperation(t, plan); ok {
		t.Fatal("ClusterCache/gantry installed Gantry without any Site")
	}
}

func TestPlanWithRacerCacheWaitsForDataplane(t *testing.T) {
	env := testEnv(t, racerCache())

	plan, res := planGantry(t, env, *siteWithGantry("edge", nil))
	if res.Ready || res.Reason != reasonRacerPending || res.RequeueAfter != racerPendingRequeue {
		t.Fatalf("result = %+v, want %s requeue", res, reasonRacerPending)
	}

	if _, ok := agentOperation(t, plan); ok {
		t.Fatal("agent DaemonSet applied before the racer-dataplane placement is known")
	}

	// Everything else is still reconciled.
	if plan.Len() == 0 {
		t.Fatal("plan is empty while waiting for Racer")
	}
}

func TestPlanRevertsWhenRacerCacheIsDeleting(t *testing.T) {
	cache := racerCache()
	cache.Finalizers = []string{"example.com/hold"}
	cache.DeletionTimestamp = ptr.To(metav1.NewTime(time.Now()))
	env := testEnv(t, cache, racerDataplane())

	plan, res := planGantry(t, env, *siteWithGantry("edge", nil))
	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	ds, _ := agentDaemonSet(t, plan)
	if _, ok := racerEnv(agentContainer(t, ds)); ok {
		t.Fatal("deleting ClusterCache/gantry kept the Racer backend")
	}
}

func TestPlanRetainedInstallationFollowsRacerCache(t *testing.T) {
	no := false
	existing := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: daemonSetName}}
	env := testEnv(t, existing, racerCache(), racerDataplane())

	plan, _ := planGantry(t, env, *siteWithGantry("edge", &no))

	ds, _ := agentDaemonSet(t, plan)
	if value, ok := racerEnv(agentContainer(t, ds)); !ok || value != "true" {
		t.Fatal("retained Gantry installation did not switch to the Racer backend")
	}
}

func TestApplyRacerModeIsIdempotent(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": daemonSetName},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"nodeSelector": map[string]any{"kubernetes.io/os": "linux"},
			"tolerations":  []any{map[string]any{"operator": "Exists"}},
			"containers": []any{
				map[string]any{"name": agentContainerName, "env": []any{map[string]any{"name": racerEnabledEnv, "value": "false"}}},
				map[string]any{"name": "sidecar"},
			},
		}}},
	}}

	// An empty dataplane placement clears Gantry's own constraints rather than
	// keeping them, so Gantry follows the dataplane exactly.
	for range 2 {
		if err := applyRacerMode(obj, &workload.NodePlacement{}); err != nil {
			t.Fatalf("applyRacerMode: %v", err)
		}
	}

	ds := &appsv1.DaemonSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, ds); err != nil {
		t.Fatalf("convert: %v", err)
	}

	pod := ds.Spec.Template.Spec
	if pod.NodeSelector != nil || pod.Tolerations != nil || pod.Affinity != nil {
		t.Fatalf("placement = %#v %#v %#v, want cleared", pod.NodeSelector, pod.Tolerations, pod.Affinity)
	}

	if len(pod.Volumes) != 2 {
		t.Fatalf("volumes = %+v, want two Racer volumes", pod.Volumes)
	}

	agent := pod.Containers[0]
	if len(agent.Env) != 1 || agent.Env[0].Value != "true" || len(agent.VolumeMounts) != 2 {
		t.Fatalf("agent = %+v, want one Racer env var and two mounts", agent)
	}

	if len(pod.Containers[1].Env) != 0 || len(pod.Containers[1].VolumeMounts) != 0 {
		t.Fatalf("sidecar was modified: %+v", pod.Containers[1])
	}
}

func TestApplyRacerModeRequiresAgentContainer(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "other"}},
		}}},
	}}

	if err := applyRacerMode(obj, &workload.NodePlacement{}); err == nil {
		t.Fatal("applyRacerMode succeeded without the agent container")
	}
}

func TestRacerCachePredicate(t *testing.T) {
	p := racerCachePredicate()
	cache := racerCache()
	other := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "other"}}

	if !p.Create(event.CreateEvent{Object: cache}) || !p.Delete(event.DeleteEvent{Object: cache}) {
		t.Fatal("ClusterCache/gantry create or delete did not reconcile Gantry")
	}

	if p.Create(event.CreateEvent{Object: other}) || p.Delete(event.DeleteEvent{Object: other}) {
		t.Fatal("another ClusterCache reconciled Gantry")
	}

	if p.Update(event.UpdateEvent{ObjectOld: cache, ObjectNew: cache.DeepCopy()}) {
		t.Fatal("a ClusterCache/gantry update without deletion reconciled Gantry")
	}

	deleting := cache.DeepCopy()
	deleting.DeletionTimestamp = ptr.To(metav1.NewTime(time.Now()))

	if !p.Update(event.UpdateEvent{ObjectOld: cache, ObjectNew: deleting}) {
		t.Fatal("ClusterCache/gantry deletion start did not reconcile Gantry")
	}
}

func racerCacheWithOrigin() *racerv1.ClusterCache {
	cache := racerCache()
	cache.Spec.Origin = &racerv1.ObjectOrigin{Azure: &racerv1.AzureOrigin{ContainerURL: "https://account.blob.core.windows.net/models"}}

	return cache
}

func TestPlanWithRacerCacheOriginKeepsDefaultMode(t *testing.T) {
	env := testEnv(t, racerCacheWithOrigin(), racerDataplane())

	plan, res := planGantry(t, env, *siteWithGantry("edge", nil))
	if res.Ready || res.Reason != reasonRacerOriginConflict {
		t.Fatalf("result = %+v, want %s", res, reasonRacerOriginConflict)
	}

	if !strings.Contains(res.Message, "spec.origin") {
		t.Fatalf("message = %q, want it to name spec.origin", res.Message)
	}

	ds, _ := agentDaemonSet(t, plan)
	if _, ok := racerEnv(agentContainer(t, ds)); ok {
		t.Fatal("ClusterCache/gantry with an origin switched Gantry to the Racer backend")
	}

	if ds.Spec.Template.Spec.NodeSelector["pool"] == "cache" {
		t.Fatal("placement copied from racer-dataplane despite the origin conflict")
	}

	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Name == racerOriginVolume || volume.Name == racerClientVolume {
			t.Fatalf("volume %s mounted despite the origin conflict", volume.Name)
		}
	}
}

func TestPlanWithRacerCacheOriginWithoutDataplane(t *testing.T) {
	env := testEnv(t, racerCacheWithOrigin())

	plan, res := planGantry(t, env, *siteWithGantry("edge", nil))
	if res.Reason != reasonRacerOriginConflict {
		t.Fatalf("result = %+v, want %s", res, reasonRacerOriginConflict)
	}

	if _, ok := agentOperation(t, plan); !ok {
		t.Fatal("agent DaemonSet not applied in default mode during the origin conflict")
	}
}

func TestRacerCachePredicateOriginChange(t *testing.T) {
	p := racerCachePredicate()

	plain := racerCache()
	plain.Generation = 1
	withOrigin := racerCacheWithOrigin()
	withOrigin.Generation = 2

	if !p.Update(event.UpdateEvent{ObjectOld: plain, ObjectNew: withOrigin}) {
		t.Fatal("adding spec.origin to ClusterCache/gantry did not reconcile Gantry")
	}

	removed := plain.DeepCopy()
	removed.Generation = 3

	if !p.Update(event.UpdateEvent{ObjectOld: withOrigin, ObjectNew: removed}) {
		t.Fatal("removing spec.origin from ClusterCache/gantry did not reconcile Gantry")
	}

	other := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "other", Generation: 1}}
	otherNew := other.DeepCopy()
	otherNew.Generation = 2

	if p.Update(event.UpdateEvent{ObjectOld: other, ObjectNew: otherNew}) {
		t.Fatal("a spec change on another ClusterCache reconciled Gantry")
	}
}
