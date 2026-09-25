// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func workloadFixture(t *testing.T) *WorkloadReconciler {
	t.Helper()
	topology := initializedTopology(t)
	cfg := topology.Config
	cfg.ControlURL = "https://racer-controller.racer.svc:8443"
	cfg.DataplaneImage = "racer:test"

	return &WorkloadReconciler{Client: topology.Client, APIReader: topology.APIReader, Config: cfg}
}

func TestWorkloadProjectionAndStorage(t *testing.T) {
	r := workloadFixture(t)

	ds, err := r.DesiredDaemonSet()
	if err != nil {
		t.Fatal(err)
	}

	pod := ds.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.ServiceAccountName != r.Config.DataplaneServiceAccount {
		t.Fatal("automatic API token or wrong service account")
	}

	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}

	token := volumes["token"].Projected.Sources[0].ServiceAccountToken
	if token.Audience != wire.TokenAudience || *token.ExpirationSeconds != 3600 {
		t.Fatal("wrong token projection")
	}

	if volumes["keyring"].Secret.SecretName != r.Config.KeyringSecretName || len(volumes["keyring"].Secret.Items) != 1 || volumes["keyring"].Secret.Items[0].Key != "bundle.json" {
		t.Fatal("not a common bounded keyring projection")
	}

	if volumes["bootstrap"].ConfigMap.Name != r.Config.BootstrapTrustConfigMap {
		t.Fatal("bootstrap trust not independent")
	}

	for _, name := range []string{"identity", "slabs", "sockets"} {
		if volumes[name].HostPath == nil || *volumes[name].HostPath.Type != corev1.HostPathDirectoryOrCreate {
			t.Fatalf("missing persistent host mount %s", name)
		}
	}

	if volumes["identity"].HostPath.Path == volumes["slabs"].HostPath.Path {
		t.Fatal("private keys share disposable storage")
	}

	requirements := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions
	if requirements[0].Key != wire.ExclusionLabel || requirements[0].Operator != corev1.NodeSelectorOpDoesNotExist {
		t.Fatal("exclusion must test presence")
	}

	for _, env := range pod.Containers[0].Env {
		if env.ValueFrom != nil {
			t.Fatal("node identity must come from verified enrollment")
		}
	}

	for _, mount := range pod.Containers[0].VolumeMounts {
		if mount.SubPath != "" {
			t.Fatal("subPath prevents projection rotation")
		}

		if (mount.Name == "token" || mount.Name == "keyring" || mount.Name == "bootstrap") && !mount.ReadOnly {
			t.Fatal("writable credential projection")
		}
	}

	if len(pod.Containers[0].Args) != 0 || len(pod.Containers[0].Command) != 0 {
		t.Fatal("workload must use the image entrypoint")
	}
}

func TestWorkloadReconcileCreateRepairAndNoop(t *testing.T) {
	r := workloadFixture(t)

	ctx := context.Background()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	if err := initialEnqueue().Start(ctx, queue); err != nil || queue.Len() != 1 {
		t.Fatalf("startup did not enqueue missing workload: %v", err)
	}

	request, _ := queue.Get()
	defer queue.Done(request)

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	ds := &appsv1.DaemonSet{}

	key := client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.DaemonSetName}
	if err := r.Get(ctx, key, ds); err != nil {
		t.Fatal(err)
	}

	ds.Annotations = map[string]string{"operator": "preserve"}
	ds.Spec.Template.Annotations = map[string]string{"rollout": "preserve"}

	ds.Spec.Template.Spec.Containers[0].Image = "drift"
	if err := r.Update(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Get(ctx, key, ds); err != nil {
		t.Fatal(err)
	}

	if ds.Spec.Template.Spec.Containers[0].Image != r.Config.DataplaneImage || ds.Annotations["operator"] != "preserve" || ds.Spec.Template.Annotations["rollout"] != "preserve" {
		t.Fatal("repair lost metadata or failed")
	}

	version := ds.ResourceVersion

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Get(ctx, key, ds); err != nil {
		t.Fatal(err)
	}

	if version != ds.ResourceVersion {
		t.Fatal("no-op wrote again")
	}
}

func TestWorkloadConflictCancellationAndOwnership(t *testing.T) {
	for _, scenario := range []string{"conflict", "cancel", "foreign", "selector", "bad URL", "missing image"} {
		t.Run(scenario, func(t *testing.T) {
			r := workloadFixture(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ds, err := r.DesiredDaemonSet()
			if err != nil {
				t.Fatal(err)
			}

			ds.Spec.Template.Spec.Containers[0].Image = "old"
			if scenario == "foreign" {
				ds.Labels = nil
			}

			if scenario == "selector" {
				ds.Spec.Selector.MatchLabels = map[string]string{"foreign": "selector"}
			}

			if err := r.Create(ctx, ds); err != nil {
				t.Fatal(err)
			}

			writes := 0
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				writes++

				data, err := patch.Data(obj)
				if err != nil {
					t.Fatal(err)
				}

				var decoded struct {
					Metadata struct {
						ResourceVersion string `json:"resourceVersion"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(data, &decoded); err != nil || decoded.Metadata.ResourceVersion == "" {
					t.Fatal("patch lacks resource-version precondition")
				}

				return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, obj.GetName(), errors.New("conflict"))
			}})

			switch scenario {
			case "cancel":
				cancel()
			case "bad URL":
				r.Config.ControlURL = "http://insecure"
			case "missing image":
				r.Config.DataplaneImage = ""
			}

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if scenario == "conflict" {
				if err != nil || result.RequeueAfter == 0 || writes != 1 {
					t.Fatalf("conflict: %v %v %d", result, err, writes)
				}
			} else if err == nil || writes != 0 {
				t.Fatalf("unsafe write: %v %d", err, writes)
			}
		})
	}
}

func TestWorkloadRepairsAddedFields(t *testing.T) {
	r := workloadFixture(t)
	ctx := context.Background()

	ds, err := r.DesiredDaemonSet()
	if err != nil {
		t.Fatal(err)
	}

	ds.Spec.Template.Spec.HostNetwork = true
	ds.Spec.Template.Spec.NodeName = "pinned-node"
	ds.Spec.Template.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true)
	ds.Spec.Template.Spec.Containers[0].Args = []string{"control"}

	ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes, corev1.Volume{Name: "unexpected", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	if err := r.Create(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		t.Fatal(err)
	}

	pod := ds.Spec.Template.Spec
	if pod.HostNetwork || pod.NodeName != "" || pod.Containers[0].SecurityContext.Privileged != nil || len(pod.Containers[0].Args) != 0 || len(pod.Volumes) != 6 {
		t.Fatal("added fields escaped drift repair")
	}
}

func TestWorkloadRetryUsesFreshRead(t *testing.T) {
	for _, createRace := range []bool{false, true} {
		t.Run(map[bool]string{false: "patch conflict", true: "create race"}[createRace], func(t *testing.T) {
			r := workloadFixture(t)
			ctx := context.Background()

			ds, err := r.DesiredDaemonSet()
			if err != nil {
				t.Fatal(err)
			}

			ds.Spec.Template.Spec.Containers[0].Image = "old"

			base := r.Client.(client.WithWatch)
			if !createRace {
				if err := base.Create(ctx, ds); err != nil {
					t.Fatal(err)
				}
			}

			writes := 0
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++

					if err := c.Create(ctx, ds); err != nil {
						return err
					}

					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, obj.GetName())
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					writes++
					if writes == 1 {
						ds.Annotations = map[string]string{"concurrent": "preserve"}
						if err := c.Update(ctx, ds); err != nil {
							return err
						}

						return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, obj.GetName(), errors.New("conflict"))
					}

					return c.Patch(ctx, obj, patch, opts...)
				},
			})

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if err != nil || result.RequeueAfter == 0 || writes != 1 {
				t.Fatalf("retry not scheduled: %v %v", result, err)
			}

			if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
				t.Fatal(err)
			}

			if err := base.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
				t.Fatal(err)
			}

			if ds.Spec.Template.Spec.Containers[0].Image != r.Config.DataplaneImage || (!createRace && ds.Annotations["concurrent"] != "preserve") || writes != 2 {
				t.Fatal("retry did not repair from current state")
			}
		})
	}
}

func TestWorkloadCancellationAfterRead(t *testing.T) {
	for _, exists := range []bool{false, true} {
		r := workloadFixture(t)
		ctx, cancel := context.WithCancel(context.Background())

		ds, err := r.DesiredDaemonSet()
		if err != nil {
			t.Fatal(err)
		}

		if exists {
			ds.Spec.Template.Spec.Containers[0].Image = "old"
			if err := r.Create(ctx, ds); err != nil {
				t.Fatal(err)
			}
		}

		r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)

			cancel()

			return err
		}})

		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				t.Fatal("create after cancellation")
				return nil
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				t.Fatal("patch after cancellation")
				return nil
			},
		})
		if _, err := r.Reconcile(ctx, ctrl.Request{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}

		cancel()
	}
}

func TestWorkloadInvalidEndpointFailsBeforeManagerStartup(t *testing.T) {
	for _, endpoint := range []string{"", "http://host", "https://host:0", "https://host:65536", "https://user@host", "https://host/path", "https://host?", "https://host/#fragment"} {
		r := workloadFixture(t)

		r.Config.ControlURL = endpoint
		if err := Run(context.Background(), r.Config); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("endpoint %q: %v", endpoint, err)
		}
	}
}

func TestWorkloadDeletionAndAPIFailure(t *testing.T) {
	r := workloadFixture(t)
	ctx := context.Background()

	ds, err := r.DesiredDaemonSet()
	if err != nil {
		t.Fatal(err)
	}

	ds.Finalizers = []string{"test.unbounded-cloud.io/hold"}
	if err := r.Create(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if err := r.Delete(ctx, ds); err != nil {
		t.Fatal(err)
	}

	result, err := r.Reconcile(ctx, ctrl.Request{})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("terminating workload: %v %v", result, err)
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		t.Fatal(err)
	}

	if ds.DeletionTimestamp.IsZero() {
		t.Fatal("deletion interrupted")
	}

	ds.Finalizers = nil
	if err := r.Update(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		t.Fatal(err)
	}

	if !ds.DeletionTimestamp.IsZero() {
		t.Fatal("workload not recreated")
	}

	apiFailure := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, ds.Name, errors.New("denied"))

	r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return apiFailure
	}})
	if _, err := r.Reconcile(ctx, ctrl.Request{}); !errors.Is(err, apiFailure) {
		t.Fatalf("API failure hidden: %v", err)
	}
}

func TestWorkloadDefaultsDoNotCauseRollout(t *testing.T) {
	r := workloadFixture(t)
	ctx := context.Background()

	ds, err := r.DesiredDaemonSet()
	if err != nil {
		t.Fatal(err)
	}
	// Defaults on the DaemonSet itself and its template metadata are supplied by
	// the API server, not workload drift. Pod defaults are explicit in desired state.
	ds.Spec.RevisionHistoryLimit = ptr.To(int32(10))

	ds.Spec.Template.CreationTimestamp = metav1.Time{}
	if err := r.Create(ctx, ds); err != nil {
		t.Fatal(err)
	}

	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		t.Fatal("default-only rollout")
		return nil
	}})
	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
}
